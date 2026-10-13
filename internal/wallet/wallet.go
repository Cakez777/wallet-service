// Package wallet contém o agregado Wallet e o lançamento de ledger.
// É domínio puro: não conhece HTTP, SQL, SQS nem Fx.
package wallet

import (
	"errors"
	"fmt"
	"time"

	"github.com/Cakez777/wallet-service/internal/money"
)

// Direction indica se um lançamento debita ou credita a carteira.
type Direction string

const (
	Debit  Direction = "DEBIT"
	Credit Direction = "CREDIT"
)

var (
	ErrInvalidWallet     = errors.New("invalid wallet")
	ErrInvalidAmount     = errors.New("invalid amount")
	ErrInsufficientFunds = errors.New("insufficient funds")
	ErrCurrencyMismatch  = errors.New("currency mismatch")
	ErrInvalidDirection  = errors.New("invalid direction")
	ErrInvalidLedger     = errors.New("invalid ledger entry")
)

// Movement descreve uma alteração de saldo já aplicada à carteira.
type Movement struct {
	Direction Direction
	Amount    money.Money
	Before    money.Money
	After     money.Money
}

// Wallet é a raiz do agregado financeiro. O estado é encapsulado: o saldo só
// muda por Apply, que preserva as invariantes (moeda igual, saldo >= 0).
type Wallet struct {
	id        string
	playerID  string
	balance   money.Money
	version   int64
	createdAt time.Time
	updatedAt time.Time
}

// New cria uma carteira nova (versão 1). O saldo inicial não pode ser negativo.
func New(id, playerID string, initial money.Money, now time.Time) (*Wallet, error) {
	if id == "" || playerID == "" {
		return nil, fmt.Errorf("%w: id and playerId are required", ErrInvalidWallet)
	}
	if initial.Currency() == "" {
		return nil, fmt.Errorf("%w: balance is not initialized", ErrInvalidWallet)
	}
	if initial.IsNegative() {
		return nil, fmt.Errorf("%w: initial balance cannot be negative", ErrInvalidWallet)
	}
	return &Wallet{id: id, playerID: playerID, balance: initial, version: 1, createdAt: now, updatedAt: now}, nil
}

// Rehydrate reconstrói uma carteira persistida. Não aplica movimentações.
func Rehydrate(id, playerID string, balance money.Money, version int64, createdAt, updatedAt time.Time) (*Wallet, error) {
	if id == "" || playerID == "" || balance.Currency() == "" {
		return nil, fmt.Errorf("%w: incomplete persisted data", ErrInvalidWallet)
	}
	if balance.IsNegative() {
		return nil, fmt.Errorf("%w: persisted balance is negative", ErrInvalidWallet)
	}
	if version < 1 {
		return nil, fmt.Errorf("%w: persisted version %d", ErrInvalidWallet, version)
	}
	return &Wallet{id: id, playerID: playerID, balance: balance, version: version, createdAt: createdAt, updatedAt: updatedAt}, nil
}

func (w *Wallet) ID() string           { return w.id }
func (w *Wallet) PlayerID() string     { return w.playerID }
func (w *Wallet) Balance() money.Money { return w.balance }
func (w *Wallet) Currency() string     { return w.balance.Currency() }
func (w *Wallet) Version() int64       { return w.version }
func (w *Wallet) CreatedAt() time.Time { return w.createdAt }
func (w *Wallet) UpdatedAt() time.Time { return w.updatedAt }

// Apply debita ou credita a carteira. A versão só é incrementada quando o saldo muda.
func (w *Wallet) Apply(dir Direction, amount money.Money, now time.Time) (Movement, error) {
	if amount.Currency() != w.balance.Currency() {
		return Movement{}, fmt.Errorf("%w: wallet %s, amount %s", ErrCurrencyMismatch, w.balance.Currency(), amount.Currency())
	}
	if !amount.IsPositive() {
		return Movement{}, fmt.Errorf("%w: must be positive", ErrInvalidAmount)
	}
	before := w.balance
	var (
		after money.Money
		err   error
	)
	switch dir {
	case Credit:
		after, err = before.Add(amount)
	case Debit:
		after, err = before.Sub(amount)
	default:
		return Movement{}, fmt.Errorf("%w: %q", ErrInvalidDirection, dir)
	}
	if err != nil {
		return Movement{}, err
	}
	if after.IsNegative() {
		return Movement{}, ErrInsufficientFunds
	}
	w.balance = after
	w.version++
	w.updatedAt = now
	return Movement{Direction: dir, Amount: amount, Before: before, After: after}, nil
}

// LedgerEntry é um lançamento imutável do ledger.
type LedgerEntry struct {
	id            string
	walletID      string
	transactionID string
	movement      Movement
	createdAt     time.Time
}

// NewLedgerEntry valida balanceAfter = balanceBefore ± amount conforme a direção.
func NewLedgerEntry(id, walletID, transactionID string, m Movement, now time.Time) (LedgerEntry, error) {
	if id == "" || walletID == "" || transactionID == "" {
		return LedgerEntry{}, fmt.Errorf("%w: ids are required", ErrInvalidLedger)
	}
	if !m.Amount.IsPositive() {
		return LedgerEntry{}, fmt.Errorf("%w: amount must be positive", ErrInvalidLedger)
	}
	var (
		expected money.Money
		err      error
	)
	switch m.Direction {
	case Credit:
		expected, err = m.Before.Add(m.Amount)
	case Debit:
		expected, err = m.Before.Sub(m.Amount)
	default:
		return LedgerEntry{}, fmt.Errorf("%w: %q", ErrInvalidDirection, m.Direction)
	}
	if err != nil {
		return LedgerEntry{}, fmt.Errorf("%w: %v", ErrInvalidLedger, err)
	}
	cmp, err := expected.Cmp(m.After)
	if err != nil || cmp != 0 {
		return LedgerEntry{}, fmt.Errorf("%w: balanceAfter does not match balanceBefore %s amount", ErrInvalidLedger, m.Direction)
	}
	return LedgerEntry{id: id, walletID: walletID, transactionID: transactionID, movement: m, createdAt: now}, nil
}

// RehydrateLedgerEntry reconstrói um lançamento persistido, revalidando a aritmética.
func RehydrateLedgerEntry(id, walletID, transactionID string, m Movement, createdAt time.Time) (LedgerEntry, error) {
	return NewLedgerEntry(id, walletID, transactionID, m, createdAt)
}

func (e LedgerEntry) ID() string            { return e.id }
func (e LedgerEntry) WalletID() string      { return e.walletID }
func (e LedgerEntry) TransactionID() string { return e.transactionID }
func (e LedgerEntry) Movement() Movement    { return e.movement }
func (e LedgerEntry) CreatedAt() time.Time  { return e.createdAt }
