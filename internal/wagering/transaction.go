package wagering

import (
	"errors"
	"fmt"
	"time"

	"github.com/Cakez777/wallet-service/internal/money"
)

// Status é o estado da transação.
type Status string

const (
	StatusPending          Status = "PENDING"           // aceito, ainda não concluído (transitório; nunca é commitado)
	StatusPendingReference Status = "PENDING_REFERENCE" // aguardando uma transação referenciada
	StatusProcessed        Status = "PROCESSED"         // sucesso; terminal
	StatusRejected         Status = "REJECTED"          // recusa de negócio; terminal
	StatusFailed           Status = "FAILED"            // falha permanente de infraestrutura; terminal
)

// IsTerminal informa se o estado não admite novas transições.
func (s Status) IsTerminal() bool {
	return s == StatusProcessed || s == StatusRejected || s == StatusFailed
}

// FailureCode é o código estável de uma rejeição ou falha.
type FailureCode string

const (
	// Rejeições definitivas de negócio.
	FailInsufficientFunds         FailureCode = "INSUFFICIENT_FUNDS"          // BET sem saldo
	FailReversalInsufficientFunds FailureCode = "REVERSAL_INSUFFICIENT_FUNDS" // reversão que debitaria mais que o saldo
	FailWalletPlayerMismatch      FailureCode = "WALLET_PLAYER_MISMATCH"      // a carteira não pertence ao jogador
	FailCurrencyMismatch          FailureCode = "CURRENCY_MISMATCH"           // moeda diferente da carteira
	FailReferenceNotFound         FailureCode = "REFERENCE_NOT_FOUND"         // referência não chegou dentro do limite
	FailReferenceNotProcessed     FailureCode = "REFERENCE_NOT_PROCESSED"     // referência rejeitada, falhou ou nunca concluiu
	FailReferenceMismatch         FailureCode = "REFERENCE_MISMATCH"          // provedor/jogador/carteira/moeda/rodada divergem
	FailReferenceKindInvalid      FailureCode = "REFERENCE_KIND_INVALID"      // tipo da referência não aceita esta operação
	FailReversalAmountMismatch    FailureCode = "REVERSAL_AMOUNT_MISMATCH"    // valor diferente do referenciado
	FailAlreadyReversed           FailureCode = "ALREADY_REVERSED"            // a referência já recebeu uma reversão
	// Falha permanente de infraestrutura.
	FailInternal FailureCode = "INTERNAL_ERROR"
)

// Origin diferencia operações externas da abertura interna.
type Origin string

const (
	OriginExternal Origin = "EXTERNAL"
	OriginInternal Origin = "INTERNAL"
)

var ErrInvalidTransition = errors.New("invalid state transition")

// Transaction é a transação de apostas (WagerTransaction). O estado é encapsulado
// e só muda por métodos de transição validados.
type Transaction struct {
	id            string
	origin        Origin
	cmd           Command
	payloadHash   string
	status        Status
	failure       FailureCode
	referenceID   string
	resultBalance *money.Money
	attempts      int
	nextAttemptAt *time.Time
	createdAt     time.Time
	updatedAt     time.Time
}

// NewTransaction cria uma transação externa em PENDING. O comando precisa ser válido.
func NewTransaction(id string, cmd Command, now time.Time) (*Transaction, error) {
	if id == "" {
		return nil, invalid("id", "is required")
	}
	if err := cmd.Validate(); err != nil {
		return nil, err
	}
	return &Transaction{
		id: id, origin: OriginExternal, cmd: cmd, payloadHash: cmd.PayloadHash(),
		status: StatusPending, createdAt: now, updatedAt: now,
	}, nil
}

// NewOpening cria a transação interna de abertura de carteira (crédito inicial positivo).
func NewOpening(id, walletID, playerID string, initial money.Money, correlationID string, now time.Time) (*Transaction, error) {
	if id == "" || walletID == "" || playerID == "" || correlationID == "" {
		return nil, invalid("opening", "id, walletId, playerId and correlationId are required")
	}
	if !initial.IsPositive() {
		return nil, invalid("opening.money", "must be greater than zero")
	}
	cmd := Command{PlayerID: playerID, WalletID: walletID, Kind: KindOpening, Money: initial, CorrelationID: correlationID}
	return &Transaction{id: id, origin: OriginInternal, cmd: cmd, status: StatusPending, createdAt: now, updatedAt: now}, nil
}

// Persisted agrupa o estado gravado, para reidratar sem reaplicar nada.
type Persisted struct {
	ID            string
	Origin        Origin
	Command       Command
	PayloadHash   string
	Status        Status
	Failure       FailureCode
	ReferenceID   string
	ResultBalance *money.Money
	Attempts      int
	NextAttemptAt *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Rehydrate reconstrói uma transação persistida. Não executa transições nem emite eventos.
func Rehydrate(p Persisted) (*Transaction, error) {
	switch p.Status {
	case StatusPending, StatusPendingReference, StatusProcessed, StatusRejected, StatusFailed:
	default:
		return nil, fmt.Errorf("persisted transaction %s has unknown status %q", p.ID, p.Status)
	}
	if p.ID == "" || p.Command.Money.Currency() == "" {
		return nil, fmt.Errorf("persisted transaction %q is incomplete", p.ID)
	}
	return &Transaction{
		id: p.ID, origin: p.Origin, cmd: p.Command, payloadHash: p.PayloadHash,
		status: p.Status, failure: p.Failure, referenceID: p.ReferenceID,
		resultBalance: p.ResultBalance, attempts: p.Attempts, nextAttemptAt: p.NextAttemptAt,
		createdAt: p.CreatedAt, updatedAt: p.UpdatedAt,
	}, nil
}

func (t *Transaction) ID() string                  { return t.id }
func (t *Transaction) Origin() Origin              { return t.origin }
func (t *Transaction) Command() Command            { return t.cmd }
func (t *Transaction) Kind() Kind                  { return t.cmd.Kind }
func (t *Transaction) PayloadHash() string         { return t.payloadHash }
func (t *Transaction) Status() Status              { return t.status }
func (t *Transaction) FailureCode() FailureCode    { return t.failure }
func (t *Transaction) ReferenceID() string         { return t.referenceID }
func (t *Transaction) ResultBalance() *money.Money { return t.resultBalance }
func (t *Transaction) Attempts() int               { return t.attempts }
func (t *Transaction) NextAttemptAt() *time.Time   { return t.nextAttemptAt }
func (t *Transaction) CreatedAt() time.Time        { return t.createdAt }
func (t *Transaction) UpdatedAt() time.Time        { return t.updatedAt }

func (t *Transaction) transitionError(to Status) error {
	return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, t.status, to)
}

func (t *Transaction) canFinish() bool {
	return t.status == StatusPending || t.status == StatusPendingReference
}

// MarkProcessed conclui com sucesso. balanceAfter é o saldo observado no processamento
// e é o que um replay devolve. referenceID é a transação referenciada, quando houver.
func (t *Transaction) MarkProcessed(balanceAfter money.Money, referenceID string, now time.Time) error {
	if !t.canFinish() {
		return t.transitionError(StatusProcessed)
	}
	t.status, t.referenceID, t.updatedAt = StatusProcessed, referenceID, now
	t.resultBalance = &balanceAfter
	t.nextAttemptAt = nil
	return nil
}

// Reject conclui com uma recusa de negócio definitiva.
func (t *Transaction) Reject(code FailureCode, referenceID string, now time.Time) error {
	if !t.canFinish() {
		return t.transitionError(StatusRejected)
	}
	if code == "" {
		return invalid("failureCode", "is required to reject")
	}
	t.status, t.failure, t.referenceID, t.updatedAt = StatusRejected, code, referenceID, now
	t.nextAttemptAt = nil
	return nil
}

// Fail registra uma falha permanente de infraestrutura, para auditoria.
func (t *Transaction) Fail(code FailureCode, now time.Time) error {
	if !t.canFinish() {
		return t.transitionError(StatusFailed)
	}
	if code == "" {
		code = FailInternal
	}
	t.status, t.failure, t.updatedAt = StatusFailed, code, now
	t.nextAttemptAt = nil
	return nil
}

// AwaitReference move PENDING para PENDING_REFERENCE e agenda a próxima tentativa.
func (t *Transaction) AwaitReference(next, now time.Time) error {
	if t.status != StatusPending {
		return t.transitionError(StatusPendingReference)
	}
	t.status, t.updatedAt = StatusPendingReference, now
	t.nextAttemptAt = &next
	return nil
}

// Reschedule registra uma nova tentativa sem sucesso e agenda a seguinte.
func (t *Transaction) Reschedule(next, now time.Time) error {
	if t.status != StatusPendingReference {
		return t.transitionError(StatusPendingReference)
	}
	t.attempts++
	t.updatedAt = now
	t.nextAttemptAt = &next
	return nil
}
