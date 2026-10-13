package wagering

import (
	"time"

	"github.com/Cakez777/wallet-service/internal/money"
	"github.com/Cakez777/wallet-service/internal/wallet"
)

// Tipos de evento de integração.
const (
	EventTransactionProcessed        = "WagerTransactionProcessed"
	EventTransactionRejected         = "WagerTransactionRejected"
	EventWalletBalanceChanged        = "WalletBalanceChanged"
	EventTransactionPendingReference = "WagerTransactionPendingReference"

	EventVersion = 1
)

// Envelope é o formato de todos os eventos. Data é um snapshot imutável.
type Envelope struct {
	EventID       string `json:"eventId"`
	EventType     string `json:"eventType"`
	AggregateID   string `json:"aggregateId"`
	CorrelationID string `json:"correlationId"`
	CausationID   string `json:"causationId,omitempty"`
	OccurredAt    string `json:"occurredAt"` // RFC 3339, UTC
	Version       int    `json:"version"`
	Data          any    `json:"data"`
}

// MoneyData é o formato monetário externo: {"amount":"25.00","currency":"BRL"}.
type MoneyData struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// ToMoneyData converte um Money para o contrato externo.
func ToMoneyData(m money.Money) MoneyData {
	return MoneyData{Amount: m.Amount(), Currency: m.Currency()}
}

// TransactionData é o payload comum dos eventos de transação. Os metadados externos
// (provedor, id externo, rodada, jogo) ficam de fora na abertura interna (OPENING).
type TransactionData struct {
	TransactionID                  string      `json:"transactionId"`
	WalletID                       string      `json:"walletId"`
	PlayerID                       string      `json:"playerId"`
	Kind                           Kind        `json:"kind"`
	Money                          MoneyData   `json:"money"`
	ProviderID                     string      `json:"providerId,omitempty"`
	ExternalTransactionID          string      `json:"externalTransactionId,omitempty"`
	RoundID                        string      `json:"roundId,omitempty"`
	GameID                         string      `json:"gameId,omitempty"`
	ReferenceExternalTransactionID string      `json:"referenceExternalTransactionId,omitempty"`
	Balance                        *MoneyData  `json:"balance,omitempty"`       // saldo observado (Processed)
	FailureCode                    FailureCode `json:"failureCode,omitempty"`   // Rejected
	NextAttemptAt                  string      `json:"nextAttemptAt,omitempty"` // PendingReference
}

// BalanceChangedData é o payload de WalletBalanceChanged.
type BalanceChangedData struct {
	WalletID      string           `json:"walletId"`
	TransactionID string           `json:"transactionId"`
	Direction     wallet.Direction `json:"direction"`
	Money         MoneyData        `json:"money"`
	BalanceBefore MoneyData        `json:"balanceBefore"`
	BalanceAfter  MoneyData        `json:"balanceAfter"`
	WalletVersion int64            `json:"walletVersion"`
}

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func txData(t *Transaction) TransactionData {
	c := t.cmd
	return TransactionData{
		TransactionID: t.id, WalletID: c.WalletID, PlayerID: c.PlayerID, Kind: c.Kind,
		Money: ToMoneyData(c.Money), ProviderID: c.ProviderID,
		ExternalTransactionID: c.ExternalTransactionID, RoundID: c.RoundID, GameID: c.GameID,
		ReferenceExternalTransactionID: c.ReferenceExternalTransactionID,
	}
}

func envelope(eventID, eventType, aggregateID string, t *Transaction, now time.Time, data any) Envelope {
	return Envelope{
		EventID: eventID, EventType: eventType, AggregateID: aggregateID,
		CorrelationID: t.cmd.CorrelationID, CausationID: t.id,
		OccurredAt: rfc3339(now), Version: EventVersion, Data: data,
	}
}

// NewProcessedEvent constrói WagerTransactionProcessed (inclui LOSS).
func NewProcessedEvent(eventID string, t *Transaction, now time.Time) Envelope {
	d := txData(t)
	if t.resultBalance != nil {
		b := ToMoneyData(*t.resultBalance)
		d.Balance = &b
	}
	return envelope(eventID, EventTransactionProcessed, t.id, t, now, d)
}

// NewRejectedEvent constrói WagerTransactionRejected.
func NewRejectedEvent(eventID string, t *Transaction, now time.Time) Envelope {
	d := txData(t)
	d.FailureCode = t.failure
	return envelope(eventID, EventTransactionRejected, t.id, t, now, d)
}

// NewPendingReferenceEvent constrói WagerTransactionPendingReference.
func NewPendingReferenceEvent(eventID string, t *Transaction, now time.Time) Envelope {
	d := txData(t)
	if t.nextAttemptAt != nil {
		d.NextAttemptAt = rfc3339(*t.nextAttemptAt)
	}
	return envelope(eventID, EventTransactionPendingReference, t.id, t, now, d)
}

// NewBalanceChangedEvent constrói WalletBalanceChanged; o agregado é a carteira.
func NewBalanceChangedEvent(eventID string, t *Transaction, m wallet.Movement, walletVersion int64, now time.Time) Envelope {
	return envelope(eventID, EventWalletBalanceChanged, t.cmd.WalletID, t, now, BalanceChangedData{
		WalletID: t.cmd.WalletID, TransactionID: t.id, Direction: m.Direction,
		Money: ToMoneyData(m.Amount), BalanceBefore: ToMoneyData(m.Before),
		BalanceAfter: ToMoneyData(m.After), WalletVersion: walletVersion,
	})
}
