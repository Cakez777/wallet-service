package wallet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/Cakez777/wallet-service/internal/money"
)

var ErrIdempotentDuplicate = errors.New("idempotent transaction already processed")

type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository {
	return &Repository{db: db}
}

func (r *Repository) BeginTx(ctx context.Context) (*sql.Tx, error) {
	return r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelDefault})
}

// Create insere uma nova carteira no banco.
func (r *Repository) Create(ctx context.Context, w *Wallet) error {
	query := `
		INSERT INTO wallets (id, player_id, currency, balance, version, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`
	_, err := r.db.ExecContext(
		ctx,
		query,
		w.ID,
		w.PlayerID,
		w.Balance.Currency(),
		w.Balance.Cents(),
		w.Version,
		w.CreatedAt,
		w.UpdatedAt,
	)
	return err
}

// GetIdempotentRecord verifica se a chave já existe e retorna o payload original gravado.
func (r *Repository) GetIdempotentRecord(ctx context.Context, idempotencyKey string) (bool, string, error) {
	if idempotencyKey == "" {
		return false, "", nil
	}

	query := `SELECT response_payload FROM transactions WHERE idempotency_key = $1`
	var payload sql.NullString
	err := r.db.QueryRowContext(ctx, query, idempotencyKey).Scan(&payload)
	if err == sql.ErrNoRows {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}

	if payload.Valid {
		return true, payload.String, nil
	}
	return true, "", nil
}

// GetByID busca a carteira por ID sem bloqueio.
func (r *Repository) GetByID(ctx context.Context, id string) (*Wallet, error) {
	query := `
		SELECT id, player_id, currency, balance, version, created_at, updated_at
		FROM wallets
		WHERE id = $1
	`
	row := r.db.QueryRowContext(ctx, query, id)

	var w Wallet
	var balanceAmount int64
	var currency string

	err := row.Scan(
		&w.ID,
		&w.PlayerID,
		&currency,
		&balanceAmount,
		&w.Version,
		&w.CreatedAt,
		&w.UpdatedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("wallet not found")
		}
		return nil, err
	}

	balance, err := money.FromCents(balanceAmount, currency)
	if err != nil {
		return nil, fmt.Errorf("invalid balance data: %w", err)
	}
	w.Balance = balance

	return &w, nil
}

func (r *Repository) GetByIDForUpdate(ctx context.Context, tx *sql.Tx, id string) (*Wallet, error) {
	query := `
		SELECT id, player_id, currency, balance, version, created_at, updated_at
		FROM wallets
		WHERE id = $1
		FOR UPDATE
	`
	row := tx.QueryRowContext(ctx, query, id)

	var w Wallet
	var balanceAmount int64
	var currency string

	err := row.Scan(
		&w.ID,
		&w.PlayerID,
		&currency,
		&balanceAmount,
		&w.Version,
		&w.CreatedAt,
		&w.UpdatedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("wallet not found")
		}
		return nil, err
	}

	balance, err := money.FromCents(balanceAmount, currency)
	if err != nil {
		return nil, fmt.Errorf("invalid balance data: %w", err)
	}
	w.Balance = balance

	return &w, nil
}

func (r *Repository) UpdateBalance(ctx context.Context, tx *sql.Tx, w *Wallet) error {
	query := `
		UPDATE wallets
		SET balance = $1, version = version + 1, updated_at = now()
		WHERE id = $2 AND version = $3
	`
	result, err := tx.ExecContext(ctx, query, w.Balance.Cents(), w.ID, w.Version)
	if err != nil {
		return err
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return fmt.Errorf("concurrency conflict: concurrent modification detected")
	}

	return nil
}

// RecordLedgerEntry insere no ledger imutável.
func (r *Repository) RecordLedgerEntry(
	ctx context.Context,
	tx *sql.Tx,
	walletID string,
	txType string,
	amount int64,
	currency string,
	balanceBefore int64,
	balanceAfter int64,
	idempotencyKey string,
	responsePayload string,
) error {
	query := `
		INSERT INTO transactions (
			id, wallet_id, type, amount, currency, 
			balance_before, balance_after, idempotency_key, response_payload
		)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, NULLIF($7, ''), $8)
	`
	_, err := tx.ExecContext(
		ctx, query,
		walletID, txType, amount, currency,
		balanceBefore, balanceAfter, idempotencyKey, responsePayload,
	)
	return err
}
