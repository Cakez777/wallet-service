package wallet

import (
	"context"
	"fmt"
	"time"

	"github.com/Cakez777/wallet-service/internal/money"
	"github.com/google/uuid"
)

// Service gerencia as regras de negócio de carteiras.
type Service struct {
	repo *Repository
}

// NewService cria uma nova instância do Service.
func NewService(repo *Repository) *Service {
	return &Service{repo: repo}
}

// CreateWallet cria uma nova carteira para o player após validar os dados.
func (s *Service) CreateWallet(ctx context.Context, playerIDStr string, currency string) (*Wallet, error) {
	// 1. Valida se o player_id é um UUID válido
	playerID, err := uuid.Parse(playerIDStr)
	if err != nil {
		return nil, fmt.Errorf("invalid player id format: %w", err)
	}

	// 2. Valida se a moeda é suportada criando um valor zero inicial
	initialBalance, err := money.Parse("0.00", currency)
	if err != nil {
		return nil, fmt.Errorf("invalid currency: %w", err)
	}

	// 3. Monta a entidade Wallet
	now := time.Now()
	w := &Wallet{
		ID:        uuid.New().String(),
		PlayerID:  playerID.String(),
		Balance:   initialBalance,
		Version:   1,
		CreatedAt: now,
		UpdatedAt: now,
	}

	// 4. Persiste no banco de dados
	if err := s.repo.Create(ctx, w); err != nil {
		return nil, fmt.Errorf("failed to create wallet in database: %w", err)
	}

	return w, nil
}

// Deposit realiza um depósito na carteira de forma atômica e segura.
func (s *Service) Deposit(ctx context.Context, walletID string, amountStr, currency string, idempotencyKey string) (*Wallet, error) {
	depositAmount, err := money.Parse(amountStr, currency)
	if err != nil {
		return nil, fmt.Errorf("invalid deposit amount: %w", err)
	}

	if !depositAmount.IsPositive() {
		return nil, fmt.Errorf("deposit amount must be positive")
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

	if w.Balance.Currency() != depositAmount.Currency() {
		return nil, fmt.Errorf("currency mismatch: wallet uses %s, deposit is in %s", w.Balance.Currency(), depositAmount.Currency())
	}

	newBalance, err := w.Balance.Add(depositAmount)
	if err != nil {
		return nil, fmt.Errorf("failed to calculate new balance: %w", err)
	}
	w.Balance = newBalance

	if err := s.repo.UpdateBalance(ctx, tx, w); err != nil {
		return nil, fmt.Errorf("failed to update balance: %w", err)
	}

	if err := s.repo.RecordTransaction(ctx, tx, w.ID, "DEPOSIT", depositAmount.Cents(), depositAmount.Currency(), idempotencyKey); err != nil {
		return nil, fmt.Errorf("failed to record transaction: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit transaction: %w", err)
	}

	return w, nil
}

// Withdraw realiza um saque na carteira, validando saldo suficiente e prevenindo saldo negativo.
func (s *Service) Withdraw(ctx context.Context, walletID string, amountStr, currency string, idempotencyKey string) (*Wallet, error) {
	withdrawAmount, err := money.Parse(amountStr, currency)
	if err != nil {
		return nil, fmt.Errorf("invalid withdrawal amount: %w", err)
	}

	if !withdrawAmount.IsPositive() {
		return nil, fmt.Errorf("withdrawal amount must be positive")
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

	if w.Balance.Currency() != withdrawAmount.Currency() {
		return nil, fmt.Errorf("currency mismatch: wallet uses %s, withdrawal is in %s", w.Balance.Currency(), withdrawAmount.Currency())
	}

	newBalance, err := w.Balance.Sub(withdrawAmount)
	if err != nil {
		return nil, fmt.Errorf("insufficient funds or calculation error: %w", err)
	}

	if newBalance.IsNegative() {
		return nil, fmt.Errorf("insufficient funds: balance cannot be negative")
	}

	w.Balance = newBalance

	if err := s.repo.UpdateBalance(ctx, tx, w); err != nil {
		return nil, fmt.Errorf("failed to update balance: %w", err)
	}

	if err := s.repo.RecordTransaction(ctx, tx, w.ID, "WITHDRAWAL", -withdrawAmount.Cents(), withdrawAmount.Currency(), idempotencyKey); err != nil {
		return nil, fmt.Errorf("failed to record transaction: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit transaction: %w", err)
	}

	return w, nil
}
