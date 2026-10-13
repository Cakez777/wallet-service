package wallet

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Cakez777/wallet-service/internal/money"
	"github.com/google/uuid"
)

type WalletResponse struct {
	Wallet           *Wallet `json:"wallet"`
	IdempotentReplay bool    `json:"idempotent_replay"`
}

type Service struct {
	repo *Repository
}

func NewService(repo *Repository) *Service {
	return &Service{repo: repo}
}

func (s *Service) CreateWallet(ctx context.Context, playerIDStr string, currency string) (*Wallet, error) {
	playerID, err := uuid.Parse(playerIDStr)
	if err != nil {
		return nil, fmt.Errorf("invalid player id format: %w", err)
	}

	initialBalance, err := money.Parse("0.00", currency)
	if err != nil {
		return nil, fmt.Errorf("invalid currency: %w", err)
	}

	now := time.Now()
	w := &Wallet{
		ID:        uuid.New().String(),
		PlayerID:  playerID.String(),
		Balance:   initialBalance,
		Version:   1,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := s.repo.Create(ctx, w); err != nil {
		return nil, fmt.Errorf("failed to create wallet in database: %w", err)
	}

	return w, nil
}

func (s *Service) Deposit(ctx context.Context, walletID string, amountStr, currency string, idempotencyKey string) (*Wallet, error) {
	res, err := s.ProcessWager(ctx, walletID, amountStr, currency, TxTypeDeposit, idempotencyKey)
	if err != nil {
		return nil, err
	}
	return res.Wallet, nil
}

func (s *Service) Withdraw(ctx context.Context, walletID string, amountStr, currency string, idempotencyKey string) (*Wallet, error) {
	res, err := s.ProcessWager(ctx, walletID, amountStr, currency, TxTypeWithdraw, idempotencyKey)
	if err != nil {
		return nil, err
	}
	return res.Wallet, nil
}

func (s *Service) Bet(ctx context.Context, walletID string, amountStr, currency string, idempotencyKey string) (*Wallet, error) {
	res, err := s.ProcessWager(ctx, walletID, amountStr, currency, TxTypeBet, idempotencyKey)
	if err != nil {
		return nil, err
	}
	return res.Wallet, nil
}

func (s *Service) Win(ctx context.Context, walletID string, amountStr, currency string, idempotencyKey string) (*Wallet, error) {
	res, err := s.ProcessWager(ctx, walletID, amountStr, currency, TxTypeWin, idempotencyKey)
	if err != nil {
		return nil, err
	}
	return res.Wallet, nil
}

func (s *Service) Loss(ctx context.Context, walletID string, amountStr, currency string, idempotencyKey string) (*Wallet, error) {
	res, err := s.ProcessWager(ctx, walletID, amountStr, currency, TxTypeLoss, idempotencyKey)
	if err != nil {
		return nil, err
	}
	return res.Wallet, nil
}

func (s *Service) Refund(ctx context.Context, walletID string, amountStr, currency string, idempotencyKey string) (*Wallet, error) {
	res, err := s.ProcessWager(ctx, walletID, amountStr, currency, TxTypeRefund, idempotencyKey)
	if err != nil {
		return nil, err
	}
	return res.Wallet, nil
}

func (s *Service) Rollback(ctx context.Context, walletID string, amountStr, currency string, idempotencyKey string) (*Wallet, error) {
	res, err := s.ProcessWager(ctx, walletID, amountStr, currency, TxTypeRollback, idempotencyKey)
	if err != nil {
		return nil, err
	}
	return res.Wallet, nil
}

func (s *Service) ProcessWager(ctx context.Context, walletID string, amountStr, currency string, txType string, idempotencyKey string) (*WalletResponse, error) {
	amount, err := money.Parse(amountStr, currency)
	if err != nil {
		return nil, fmt.Errorf("invalid amount: %w", err)
	}
	if !amount.IsPositive() {
		return nil, fmt.Errorf("amount must be positive")
	}

	if idempotencyKey != "" {
		exists, payloadStr, err := s.repo.GetIdempotentRecord(ctx, idempotencyKey)
		if err == nil && exists {
			var wallet Wallet
			if payloadStr != "" {
				_ = json.Unmarshal([]byte(payloadStr), &wallet)
			} else {
				if wCurrent, err := s.repo.GetByID(ctx, walletID); err == nil {
					wallet = *wCurrent
				}
			}
			return &WalletResponse{
				Wallet:           &wallet,
				IdempotentReplay: true,
			}, nil
		}
	}

	tx, err := s.repo.BeginTx(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	w, err := s.repo.GetByIDForUpdate(ctx, tx, walletID)
	if err != nil {
		return nil, err
	}

	if w.Balance.Currency() != amount.Currency() {
		return nil, fmt.Errorf("currency mismatch: wallet uses %s, transaction is in %s", w.Balance.Currency(), amount.Currency())
	}

	balanceBefore := w.Balance.Cents()
	var newBalance money.Money

	switch txType {
	case TxTypeBet, TxTypeLoss, TxTypeWithdraw:
		newBalance, err = w.Balance.Sub(amount)
		if err != nil || newBalance.IsNegative() {
			return nil, fmt.Errorf("insufficient funds")
		}
	case TxTypeWin, TxTypeRefund, TxTypeRollback, TxTypeDeposit:
		newBalance, err = w.Balance.Add(amount)
		if err != nil {
			return nil, fmt.Errorf("calculation error: %w", err)
		}
	default:
		return nil, fmt.Errorf("unknown transaction type: %s", txType)
	}

	balanceAfter := newBalance.Cents()
	w.Balance = newBalance

	if err := s.repo.UpdateBalance(ctx, tx, w); err != nil {
		return nil, fmt.Errorf("failed to update balance: %w", err)
	}

	payloadBytes, _ := json.Marshal(w)
	payloadStr := string(payloadBytes)

	rawAmount := amount.Cents()
	if txType == TxTypeBet || txType == TxTypeLoss || txType == TxTypeWithdraw {
		rawAmount = -rawAmount
	}

	if err := s.repo.RecordLedgerEntry(ctx, tx, walletID, txType, rawAmount, currency, balanceBefore, balanceAfter, idempotencyKey, payloadStr); err != nil {
		return nil, fmt.Errorf("failed to record ledger entry: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit transaction: %w", err)
	}

	return &WalletResponse{
		Wallet:           w,
		IdempotentReplay: false,
	}, nil
}
