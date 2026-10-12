package wallet

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/Cakez777/wallet-service/internal/money"
)

// Repository gerencia a persistência de wallets.
type Repository struct {
	db *sql.DB
}

// NewRepository cria uma nova instância do Repository.
func NewRepository(db *sql.DB) *Repository {
	return &Repository{db: db}
}

// BeginTx inicia uma transação no banco de dados.
func (r *Repository) BeginTx(ctx context.Context) (*sql.Tx, error) {
	return r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
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
		w.Balance.Cents(), // <-- Alterado de Amount() para Cents()
		w.Version,
		w.CreatedAt,
		w.UpdatedAt,
	)
	return err
}

// GetByIDForUpdate busca a carteira por ID aplicando bloqueio de linha (FOR UPDATE) dentro de uma transação.
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

	// Hidrata o objeto Money usando o FromCents do pacote money
	balance, err := money.FromCents(balanceAmount, currency)
	if err != nil {
		return nil, fmt.Errorf("invalid balance data from database: %w", err)
	}
	w.Balance = balance

	return &w, nil
}

// UpdateBalance atualiza o saldo e incrementa a versão da carteira de forma atômica dentro da transação.
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
		return fmt.Errorf("concurrency conflict: wallet was modified concurrently")
	}

	return nil
}

// RecordTransaction registra a movimentação na tabela transactions para fins de ledger e auditoria.
func (r *Repository) RecordTransaction(ctx context.Context, tx *sql.Tx, walletID, txType string, amount int64, currency, idempotencyKey string) error {
	query := `
		INSERT INTO transactions (id, wallet_id, type, amount, currency, idempotency_key)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, NULLIF($5, ''))
	`
	_, err := tx.ExecContext(ctx, query, walletID, txType, amount, currency, idempotencyKey)
	return err
}
