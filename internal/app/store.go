package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Cakez777/wallet-service/internal/money"
	"github.com/Cakez777/wallet-service/internal/wagering"
	"github.com/Cakez777/wallet-service/internal/wallet"
)

// Funções de acesso ao PostgreSQL. Todo SQL é explícito; as transações são abertas
// pelos casos de uso (Service) e passadas como *sql.Tx.

const txColumns = `id, origin, kind, status, wallet_id, player_id, amount, currency,
	provider_id, external_transaction_id, idempotency_key, payload_hash, round_id, game_id,
	reference_external_transaction_id, reference_transaction_id, failure_code,
	result_balance_after, correlation_id, attempts, next_attempt_at, created_at, updated_at`

type scanner interface{ Scan(dest ...any) error }

func scanTx(row scanner) (*wagering.Transaction, error) {
	var (
		id, origin, kind, status, walletID, playerID, currency, correlation string
		amount                                                              int64
		provider, extID, idemKey, hash, round, game                         sql.NullString
		refExt, refID, failure                                              sql.NullString
		resultBal                                                           sql.NullInt64
		attempts                                                            int
		next                                                                sql.NullTime
		created, updated                                                    time.Time
	)
	if err := row.Scan(&id, &origin, &kind, &status, &walletID, &playerID, &amount, &currency,
		&provider, &extID, &idemKey, &hash, &round, &game, &refExt, &refID, &failure,
		&resultBal, &correlation, &attempts, &next, &created, &updated); err != nil {
		return nil, err
	}
	cur := strings.TrimSpace(currency)
	m, err := money.FromCents(amount, cur)
	if err != nil {
		return nil, fmt.Errorf("transaction %s: %w", id, err)
	}
	p := wagering.Persisted{
		ID: id, Origin: wagering.Origin(origin), Status: wagering.Status(status),
		Failure: wagering.FailureCode(failure.String), ReferenceID: refID.String, PayloadHash: hash.String,
		Attempts: attempts, CreatedAt: created, UpdatedAt: updated,
		Command: wagering.Command{
			ProviderID: provider.String, ExternalTransactionID: extID.String, IdempotencyKey: idemKey.String,
			PlayerID: playerID, WalletID: walletID, RoundID: round.String, GameID: game.String,
			Kind: wagering.Kind(kind), Money: m, ReferenceExternalTransactionID: refExt.String,
			CorrelationID: correlation,
		},
	}
	if resultBal.Valid {
		rb, err := money.FromCents(resultBal.Int64, cur)
		if err != nil {
			return nil, err
		}
		p.ResultBalance = &rb
	}
	if next.Valid {
		t := next.Time
		p.NextAttemptAt = &t
	}
	return wagering.Rehydrate(p)
}

func nullStr(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

func insertTx(ctx context.Context, tx *sql.Tx, t *wagering.Transaction) error {
	c := t.Command()
	var resultBal sql.NullInt64
	if rb := t.ResultBalance(); rb != nil {
		resultBal = sql.NullInt64{Int64: rb.Cents(), Valid: true}
	}
	var next sql.NullTime
	if n := t.NextAttemptAt(); n != nil {
		next = sql.NullTime{Time: *n, Valid: true}
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO wager_transactions (`+txColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)`,
		t.ID(), string(t.Origin()), string(c.Kind), string(t.Status()), c.WalletID, c.PlayerID,
		c.Money.Cents(), c.Money.Currency(),
		nullStr(c.ProviderID), nullStr(c.ExternalTransactionID), nullStr(c.IdempotencyKey), nullStr(t.PayloadHash()),
		nullStr(c.RoundID), nullStr(c.GameID), nullStr(c.ReferenceExternalTransactionID),
		nullStr(t.ReferenceID()), nullStr(string(t.FailureCode())), resultBal, c.CorrelationID,
		t.Attempts(), next, t.CreatedAt(), t.UpdatedAt())
	return err
}

func updateTx(ctx context.Context, tx *sql.Tx, t *wagering.Transaction) error {
	var resultBal sql.NullInt64
	if rb := t.ResultBalance(); rb != nil {
		resultBal = sql.NullInt64{Int64: rb.Cents(), Valid: true}
	}
	var next sql.NullTime
	if n := t.NextAttemptAt(); n != nil {
		next = sql.NullTime{Time: *n, Valid: true}
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE wager_transactions
		   SET status=$2, failure_code=$3, result_balance_after=$4, reference_transaction_id=$5,
		       attempts=$6, next_attempt_at=$7, updated_at=$8
		 WHERE id=$1`,
		t.ID(), string(t.Status()), nullStr(string(t.FailureCode())), resultBal,
		nullStr(t.ReferenceID()), t.Attempts(), next, t.UpdatedAt())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("update wager transaction %s: %d rows affected", t.ID(), n)
	}
	return nil
}

func scanWallet(row scanner) (*wallet.Wallet, error) {
	var (
		id, playerID, currency string
		balance, version       int64
		created, updated       time.Time
	)
	if err := row.Scan(&id, &playerID, &currency, &balance, &version, &created, &updated); err != nil {
		return nil, err
	}
	m, err := money.FromCents(balance, strings.TrimSpace(currency))
	if err != nil {
		return nil, err
	}
	return wallet.Rehydrate(id, playerID, m, version, created, updated)
}

const walletColumns = `id, player_id, currency, balance, version, created_at, updated_at`

// lockWallet trava a linha da carteira (FOR UPDATE): é a coordenação por carteira.
func lockWallet(ctx context.Context, tx *sql.Tx, id string) (*wallet.Wallet, error) {
	w, err := scanWallet(tx.QueryRowContext(ctx, `SELECT `+walletColumns+` FROM wallets WHERE id=$1 FOR UPDATE`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrWalletNotFound
	}
	return w, err
}

// updateWallet grava o novo saldo; WHERE version=... é a segunda barreira contra lost updates.
func updateWallet(ctx context.Context, tx *sql.Tx, w *wallet.Wallet) error {
	res, err := tx.ExecContext(ctx,
		`UPDATE wallets SET balance=$2, version=$3, updated_at=$4 WHERE id=$1 AND version=$5`,
		w.ID(), w.Balance().Cents(), w.Version(), w.UpdatedAt(), w.Version()-1)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("wallet %s: concurrent modification detected", w.ID())
	}
	return nil
}

func insertLedger(ctx context.Context, tx *sql.Tx, e wallet.LedgerEntry) error {
	m := e.Movement()
	_, err := tx.ExecContext(ctx, `
		INSERT INTO wallet_ledger_entries
		  (id, wallet_id, transaction_id, direction, amount, currency, balance_before, balance_after, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		e.ID(), e.WalletID(), e.TransactionID(), string(m.Direction), m.Amount.Cents(), m.Amount.Currency(),
		m.Before.Cents(), m.After.Cents(), e.CreatedAt())
	return err
}

func insertEvent(ctx context.Context, tx *sql.Tx, env wagering.Envelope, aggregateType string) error {
	payload, err := json.Marshal(env)
	if err != nil {
		return err
	}
	occurred, err := time.Parse(time.RFC3339Nano, env.OccurredAt)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO outbox_events (event_id, aggregate_id, aggregate_type, event_type, payload, occurred_at, next_attempt_at)
		VALUES ($1,$2,$3,$4,$5,$6,$6)`,
		env.EventID, env.AggregateID, aggregateType, env.EventType, payload, occurred)
	return err
}

// findExisting procura por chave de idempotência e por id externo do mesmo provedor.
func findExisting(ctx context.Context, tx *sql.Tx, c wagering.Command) (byKey, byExternal *wagering.Transaction, err error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT `+txColumns+` FROM wager_transactions
		 WHERE origin='EXTERNAL' AND provider_id=$1 AND (idempotency_key=$2 OR external_transaction_id=$3)`,
		c.ProviderID, c.IdempotencyKey, c.ExternalTransactionID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		t, err := scanTx(rows)
		if err != nil {
			return nil, nil, err
		}
		if t.Command().IdempotencyKey == c.IdempotencyKey {
			byKey = t
		} else {
			byExternal = t
		}
	}
	return byKey, byExternal, rows.Err()
}

// lookupReference resolve (providerId, referenceExternalTransactionId). nil quando não existe.
func lookupReference(ctx context.Context, tx *sql.Tx, c wagering.Command) (*wagering.Reference, error) {
	if c.ReferenceExternalTransactionID == "" {
		return nil, nil
	}
	t, err := scanTx(tx.QueryRowContext(ctx, `
		SELECT `+txColumns+` FROM wager_transactions
		 WHERE origin='EXTERNAL' AND provider_id=$1 AND external_transaction_id=$2`,
		c.ProviderID, c.ReferenceExternalTransactionID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var reversed bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM wager_transactions
		                WHERE reference_transaction_id=$1 AND kind IN ('REFUND','ROLLBACK') AND status='PROCESSED')`,
		t.ID()).Scan(&reversed); err != nil {
		return nil, err
	}
	rc := t.Command()
	return &wagering.Reference{
		ID: t.ID(), Kind: rc.Kind, Status: t.Status(), PlayerID: rc.PlayerID, WalletID: rc.WalletID,
		RoundID: rc.RoundID, Money: rc.Money, AlreadyReversed: reversed,
	}, nil
}

func currencyOf(s string) string { return strings.TrimSpace(s) }
