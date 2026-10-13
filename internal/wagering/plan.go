package wagering

import (
	"github.com/Cakez777/wallet-service/internal/money"
	"github.com/Cakez777/wallet-service/internal/wallet"
)

// WalletView é o que a decisão precisa saber da carteira.
type WalletView struct {
	PlayerID string
	Balance  money.Money
}

// Reference é a transação referenciada, resolvida por (providerId, referenceExternalTransactionId).
type Reference struct {
	ID              string
	Kind            Kind
	Status          Status
	PlayerID        string
	WalletID        string
	RoundID         string
	Money           money.Money
	AlreadyReversed bool // já existe REFUND/ROLLBACK PROCESSED apontando para ela
}

// Action é o resultado da decisão.
type Action int

const (
	// ActionApply: concluir com sucesso (com ou sem movimentação).
	ActionApply Action = iota
	// ActionReject: recusar definitivamente com Failure.
	ActionReject
	// ActionWait: a referência ainda não está disponível; tentar mais tarde.
	ActionWait
)

// Outcome é a decisão tomada por Plan.
type Outcome struct {
	Action Action
	// Direction é a movimentação a aplicar (ActionApply). Vazia para LOSS, sem movimentação.
	// O valor movimentado é sempre Command.Money.
	Direction wallet.Direction
	// Failure é o código da rejeição (ActionReject) ou, em ActionWait, o código a usar
	// se as tentativas se esgotarem.
	Failure FailureCode
}

func reject(code FailureCode) Outcome { return Outcome{Action: ActionReject, Failure: code} }

// Plan decide o que fazer com um comando, sem efeitos colaterais. ref é nil quando a
// transação referenciada não existe (ou o comando não referencia nada).
//
// Regras por tipo:
//
//	BET      débito; exige saldo suficiente.
//	WIN      crédito; referência opcional deve ser uma BET.
//	LOSS     sem movimentação; referência opcional deve ser uma BET.
//	REFUND   crédito do valor integral de uma BET processada.
//	ROLLBACK desfaz integralmente uma BET (crédito), WIN (débito) ou REFUND (débito).
//
// Uma transação recebe no máximo uma reversão bem-sucedida (REFUND ou ROLLBACK);
// a segunda é rejeitada com ALREADY_REVERSED, o que impede devolver o mesmo débito duas vezes.
func Plan(cmd Command, w WalletView, ref *Reference) Outcome {
	if w.PlayerID != cmd.PlayerID {
		return reject(FailWalletPlayerMismatch)
	}
	if cmd.Money.Currency() != w.Balance.Currency() {
		return reject(FailCurrencyMismatch)
	}

	if cmd.ReferenceExternalTransactionID != "" {
		if out, stop := checkReference(cmd, ref); stop {
			return out
		}
	}

	switch cmd.Kind {
	case KindBet:
		if cmp, err := w.Balance.Cmp(cmd.Money); err != nil || cmp < 0 {
			return reject(FailInsufficientFunds)
		}
		return Outcome{Action: ActionApply, Direction: wallet.Debit}
	case KindWin:
		return Outcome{Action: ActionApply, Direction: wallet.Credit}
	case KindLoss:
		return Outcome{Action: ActionApply}
	case KindRefund:
		return Outcome{Action: ActionApply, Direction: wallet.Credit}
	case KindRollback:
		if ref.Kind == KindBet {
			return Outcome{Action: ActionApply, Direction: wallet.Credit}
		}
		// WIN ou REFUND: desfazer significa debitar.
		if cmp, err := w.Balance.Cmp(cmd.Money); err != nil || cmp < 0 {
			return reject(FailReversalInsufficientFunds)
		}
		return Outcome{Action: ActionApply, Direction: wallet.Debit}
	}
	return reject(FailInternal)
}

// checkReference valida a referência. stop=true quando a decisão já está tomada.
func checkReference(cmd Command, ref *Reference) (Outcome, bool) {
	if ref == nil {
		return Outcome{Action: ActionWait, Failure: FailReferenceNotFound}, true
	}
	switch ref.Status {
	case StatusRejected, StatusFailed:
		return reject(FailReferenceNotProcessed), true
	case StatusPending, StatusPendingReference:
		// Existe, mas ainda não concluiu: espera. Se esgotar, o código é NOT_PROCESSED.
		return Outcome{Action: ActionWait, Failure: FailReferenceNotProcessed}, true
	}

	if ref.PlayerID != cmd.PlayerID || ref.WalletID != cmd.WalletID ||
		ref.RoundID != cmd.RoundID || ref.Money.Currency() != cmd.Money.Currency() {
		return reject(FailReferenceMismatch), true
	}

	switch cmd.Kind {
	case KindWin, KindLoss, KindRefund:
		if ref.Kind != KindBet {
			return reject(FailReferenceKindInvalid), true
		}
	case KindRollback:
		if ref.Kind != KindBet && ref.Kind != KindWin && ref.Kind != KindRefund {
			return reject(FailReferenceKindInvalid), true
		}
	}

	if cmd.Kind == KindRefund || cmd.Kind == KindRollback {
		if cmp, err := cmd.Money.Cmp(ref.Money); err != nil || cmp != 0 {
			return reject(FailReversalAmountMismatch), true
		}
		if ref.AlreadyReversed {
			return reject(FailAlreadyReversed), true
		}
	}
	return Outcome{}, false
}
