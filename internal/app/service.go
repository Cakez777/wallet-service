// Package app contém os casos de uso e a persistência em PostgreSQL.
package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Cakez777/wallet-service/internal/money"
	"github.com/Cakez777/wallet-service/internal/wagering"
	"github.com/Cakez777/wallet-service/internal/wallet"
	"github.com/google/uuid"
)

// Config da retentativa de operações que aguardam uma referência.
type Config struct {
	PendingBaseDelay   time.Duration // atraso da 1ª tentativa; dobra a cada tentativa
	PendingMaxDelay    time.Duration // teto do backoff
	PendingMaxAttempts int           // ao atingir, a transação é rejeitada
}

func (c Config) withDefaults() Config {
	if c.PendingBaseDelay <= 0 {
		c.PendingBaseDelay = 2 * time.Second
	}
	if c.PendingMaxDelay <= 0 {
		c.PendingMaxDelay = 5 * time.Minute
	}
	if c.PendingMaxAttempts <= 0 {
		c.PendingMaxAttempts = 8
	}
	return c
}

// Service implementa os casos de uso. É seguro para uso concorrente e entre várias instâncias:
// o estado vive no PostgreSQL e a coordenação é por carteira (SELECT ... FOR UPDATE).
type Service struct {
	db    *sql.DB
	cfg   Config
	now   func() time.Time
	newID func() string
	obs   Observer
}

// NewService cria o serviço sobre um pool de conexões.
func NewService(db *sql.DB, cfg Config) *Service {
	return &Service{
		db: db, cfg: cfg.withDefaults(),
		now:   func() time.Time { return time.Now().UTC() },
		newID: func() string { return uuid.Must(uuid.NewV7()).String() },
		obs:   noopObserver{},
	}
}

// WithObserver devolve uma cópia que reporta eventos operacionais (métricas) ao observador.
func (s *Service) WithObserver(o Observer) *Service {
	c := *s
	if o == nil {
		o = noopObserver{}
	}
	c.obs = o
	return &c
}

// WithClock devolve uma cópia com relógio próprio (usado em testes).
func (s *Service) WithClock(now func() time.Time) *Service {
	c := *s
	c.now = now
	return &c
}

// Result é o resultado de Process.
type Result struct {
	Transaction *wagering.Transaction
	Replay      bool // true quando devolve o resultado persistido de uma entrega repetida
}

const maxAttempts = 3

// Process aplica uma operação externa (HTTP ou SQS) com idempotência persistente.
//
// Tudo ocorre em uma única transação SQL: trava da carteira, consulta de duplicata,
// decisão, linha da transação, lançamento no ledger, saldo e eventos do outbox.
// Erros de negócio viram transações REJECTED (persistidas e auditáveis); entradas
// inválidas retornam *wagering.ValidationError sem persistir nada.
func (s *Service) Process(ctx context.Context, cmd wagering.Command) (Result, error) {
	return s.ProcessWith(ctx, cmd, Hooks{})
}

// Hooks permitem ao chamador (o consumidor SQS) participar da MESMA transação SQL do
// processamento, sem que o serviço conheça mensageria.
type Hooks struct {
	// Source identifica a origem nas métricas ("http" quando vazio).
	Source string
	// Before roda logo após abrir a transação, antes de travar a carteira. Retornando
	// skip=true, nada é processado e a transação é desfeita (entrega duplicada).
	Before func(ctx context.Context, tx *sql.Tx) (skip bool, err error)
	// After roda antes do commit, depois que a operação foi decidida e gravada.
	After func(ctx context.Context, tx *sql.Tx, res Result) error
}

// ProcessWith é Process com ganchos transacionais. Se Before pedir skip, devolve
// Result{Replay: true} sem Transaction.
func (s *Service) ProcessWith(ctx context.Context, cmd wagering.Command, h Hooks) (Result, error) {
	if err := cmd.Validate(); err != nil {
		return Result{}, err
	}
	source := h.Source
	if source == "" {
		source = "http"
	}
	started := time.Now()
	var lastErr error
	for i := 0; i < maxAttempts; i++ {
		res, err := s.processOnce(ctx, cmd, h)
		// Unique violation: outra instância gravou a mesma operação por outra carteira/caminho;
		// a nova tentativa a encontrará e responderá como replay ou conflito.
		if err != nil && (isUniqueViolation(err) || isRetryable(err)) {
			lastErr = err
			s.obs.ConcurrencyConflict()
			continue
		}
		status := wagering.Status("ERROR")
		if err == nil && res.Transaction != nil {
			status = res.Transaction.Status()
		} else if err == nil {
			status = "DUPLICATE_SKIPPED"
		}
		s.obs.Result(source, cmd.Kind, status, res.Replay, time.Since(started))
		return res, err
	}
	s.obs.Result(source, cmd.Kind, "ERROR", false, time.Since(started))
	return Result{}, fmt.Errorf("process: giving up after %d attempts: %w", maxAttempts, lastErr)
}

func (s *Service) processOnce(ctx context.Context, cmd wagering.Command, h Hooks) (Result, error) {
	tx, err := s.db.BeginTx(ctx, nil) // READ COMMITTED: o lock de linha serializa a carteira
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback()

	if h.Before != nil {
		skip, err := h.Before(ctx, tx)
		if err != nil {
			return Result{}, err
		}
		if skip {
			return Result{Replay: true}, nil
		}
	}

	w, err := lockWallet(ctx, tx, cmd.WalletID)
	if err != nil {
		return Result{}, err
	}

	byKey, byExternal, err := findExisting(ctx, tx, cmd)
	if err != nil {
		return Result{}, err
	}
	if byKey != nil {
		if byKey.PayloadHash() != cmd.PayloadHash() {
			return Result{}, ErrIdempotencyConflict
		}
		if h.After != nil {
			if err := h.After(ctx, tx, Result{Transaction: byKey, Replay: true}); err != nil {
				return Result{}, err
			}
			if err := tx.Commit(); err != nil {
				return Result{}, err
			}
		}
		return Result{Transaction: byKey, Replay: true}, nil
	}
	if byExternal != nil {
		return Result{}, ErrExternalTransactionConflict
	}

	t, err := wagering.NewTransaction(s.newID(), cmd, s.now())
	if err != nil {
		return Result{}, err
	}
	ref, err := lookupReference(ctx, tx, cmd)
	if err != nil {
		return Result{}, err
	}
	out := wagering.Plan(cmd, wagering.WalletView{PlayerID: w.PlayerID(), Balance: w.Balance()}, ref)
	if err := s.settle(ctx, tx, w, t, out, ref, false); err != nil {
		return Result{}, err
	}
	if h.After != nil {
		if err := h.After(ctx, tx, Result{Transaction: t}); err != nil {
			return Result{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Result{}, err
	}
	return Result{Transaction: t}, nil
}

func (s *Service) delay(attempt int) time.Duration {
	d := s.cfg.PendingBaseDelay
	max := s.cfg.PendingMaxDelay

	if attempt <= 1 {
		if d > max {
			return max
		}
		return d
	}

	for i := 1; i < attempt && d < max; i++ {
		// Evita overflow ao dobrar a duração.
		if d > max/2 {
			return max
		}
		d *= 2
	}

	if d > max {
		return max
	}
	return d
}

// settle aplica a decisão: transição de estado, linha da transação, ledger, saldo e eventos.
// existing=true quando a linha já existe (retentativa de PENDING_REFERENCE): faz UPDATE.
func (s *Service) settle(ctx context.Context, tx *sql.Tx, w *wallet.Wallet, t *wagering.Transaction,
	out wagering.Outcome, ref *wagering.Reference, existing bool) error {

	now := s.now()
	refID := ""
	if ref != nil {
		refID = ref.ID
	}
	persist := func() error {
		if existing {
			return updateTx(ctx, tx, t)
		}
		return insertTx(ctx, tx, t)
	}
	reject := func(code wagering.FailureCode) error {
		if err := t.Reject(code, refID, now); err != nil {
			return err
		}
		if err := persist(); err != nil {
			return err
		}
		return insertEvent(ctx, tx, wagering.NewRejectedEvent(s.newID(), t, now), "WagerTransaction")
	}

	switch out.Action {
	case wagering.ActionReject:
		return reject(out.Failure)

	case wagering.ActionWait:
		if !existing {
			if err := t.AwaitReference(now.Add(s.delay(1)), now); err != nil {
				return err
			}
			if err := persist(); err != nil {
				return err
			}
			return insertEvent(ctx, tx, wagering.NewPendingReferenceEvent(s.newID(), t, now), "WagerTransaction")
		}
		if t.Attempts()+1 >= s.cfg.PendingMaxAttempts {
			return reject(out.Failure) // tentativas esgotadas
		}
		if err := t.Reschedule(now.Add(s.delay(t.Attempts()+2)), now); err != nil {
			return err
		}
		return persist()

	case wagering.ActionApply:
		var mv *wallet.Movement
		if out.Direction != "" {
			m, err := w.Apply(out.Direction, t.Command().Money, now)
			if err != nil {
				return fmt.Errorf("apply movement: %w", err)
			}
			mv = &m
		}
		if err := t.MarkProcessed(w.Balance(), refID, now); err != nil {
			return err
		}
		if err := persist(); err != nil {
			return err
		}
		if mv != nil {
			entry, err := wallet.NewLedgerEntry(s.newID(), w.ID(), t.ID(), *mv, now)
			if err != nil {
				return err
			}
			if err := insertLedger(ctx, tx, entry); err != nil {
				return err
			}
			if err := updateWallet(ctx, tx, w); err != nil {
				return err
			}
		}
		if err := insertEvent(ctx, tx, wagering.NewProcessedEvent(s.newID(), t, now), "WagerTransaction"); err != nil {
			return err
		}
		if mv != nil {
			return insertEvent(ctx, tx, wagering.NewBalanceChangedEvent(s.newID(), t, *mv, w.Version(), now), "Wallet")
		}
		return nil
	}
	return fmt.Errorf("unknown outcome action %d", out.Action)
}

// CreateWallet abre uma carteira. Com saldo positivo, cria a transação OPENING (PROCESSED),
// o lançamento de crédito e os eventos, tudo no mesmo commit. A versão inicial é 1.
func (s *Service) CreateWallet(ctx context.Context, playerID string, initial money.Money, correlationID string) (*wallet.Wallet, error) {
	if !wagering.IsUUID(playerID) {
		return nil, &wagering.ValidationError{Field: "playerId", Reason: "must be a UUID"}
	}
	if initial.Currency() == "" {
		return nil, &wagering.ValidationError{Field: "initialBalance", Reason: "is required"}
	}
	if initial.IsNegative() {
		return nil, &wagering.ValidationError{Field: "initialBalance.amount", Reason: "cannot be negative"}
	}
	if correlationID == "" {
		correlationID = s.newID()
	}
	now := s.now()
	w, err := wallet.New(s.newID(), playerID, initial, now)
	if err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx,
		`INSERT INTO wallets (id, player_id, currency, balance, version, created_at, updated_at) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		w.ID(), w.PlayerID(), w.Currency(), w.Balance().Cents(), w.Version(), w.CreatedAt(), w.UpdatedAt())
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrWalletAlreadyExists
		}
		return nil, err
	}

	if initial.IsPositive() {
		t, err := wagering.NewOpening(s.newID(), w.ID(), w.PlayerID(), initial, correlationID, now)
		if err != nil {
			return nil, err
		}
		if err := t.MarkProcessed(initial, "", now); err != nil {
			return nil, err
		}
		zero, _ := money.Zero(initial.Currency())
		mv := wallet.Movement{Direction: wallet.Credit, Amount: initial, Before: zero, After: initial}
		entry, err := wallet.NewLedgerEntry(s.newID(), w.ID(), t.ID(), mv, now)
		if err != nil {
			return nil, err
		}
		if err := insertTx(ctx, tx, t); err != nil {
			return nil, err
		}
		if err := insertLedger(ctx, tx, entry); err != nil {
			return nil, err
		}
		if err := insertEvent(ctx, tx, wagering.NewProcessedEvent(s.newID(), t, now), "WagerTransaction"); err != nil {
			return nil, err
		}
		if err := insertEvent(ctx, tx, wagering.NewBalanceChangedEvent(s.newID(), t, mv, w.Version(), now), "Wallet"); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return w, nil
}

// ---------------------------------------------------------------------------
// Referências pendentes
// ---------------------------------------------------------------------------

// ProcessPendingBatch retenta transações PENDING_REFERENCE vencidas. Pode rodar em várias
// instâncias ao mesmo tempo: cada retentativa trava a carteira e reconfere o estado.
func (s *Service) ProcessPendingBatch(ctx context.Context, limit int) (int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM wager_transactions
		 WHERE status='PENDING_REFERENCE' AND next_attempt_at <= $1
		 ORDER BY next_attempt_at LIMIT $2`, s.now(), limit)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	handled := 0
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return handled, err
		}
		var rerr error
		for i := 0; i < maxAttempts; i++ {
			rerr = s.retryPending(ctx, id)
			if rerr == nil || !isRetryable(rerr) {
				break
			}
		}
		if rerr != nil {
			slog.Error("falha ao retentar transação pendente", "transactionId", id, "error", rerr)
			continue
		}
		handled++
	}
	return handled, nil
}

func (s *Service) retryPending(ctx context.Context, id string) error {
	now := s.now()
	var walletID string
	err := s.db.QueryRowContext(ctx,
		`SELECT wallet_id FROM wager_transactions WHERE id=$1 AND status='PENDING_REFERENCE' AND next_attempt_at <= $2`,
		id, now).Scan(&walletID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // outra instância já tratou
	}
	if err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	w, err := lockWallet(ctx, tx, walletID) // sempre carteira antes da transação: sem deadlock
	if err != nil {
		return err
	}
	t, err := scanTx(tx.QueryRowContext(ctx, `SELECT `+txColumns+` FROM wager_transactions WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return err
	}
	if t.Status() != wagering.StatusPendingReference || t.NextAttemptAt() == nil || t.NextAttemptAt().After(s.now()) {
		return nil // já resolvida ou reagendada por outra instância
	}
	cmd := t.Command()
	ref, err := lookupReference(ctx, tx, cmd)
	if err != nil {
		return err
	}
	out := wagering.Plan(cmd, wagering.WalletView{PlayerID: w.PlayerID(), Balance: w.Balance()}, ref)
	if err := s.settle(ctx, tx, w, t, out, ref, true); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.obs.PendingRetry()
	return nil
}

// RunPendingWorker retenta pendências periodicamente até ctx ser cancelado.
func (s *Service) RunPendingWorker(ctx context.Context, interval time.Duration, batch int) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		for {
			n, err := s.ProcessPendingBatch(ctx, batch)
			if err != nil {
				if ctx.Err() == nil {
					slog.Error("worker de referências pendentes", "error", err)
				}
				break
			}
			if n < batch {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// ---------------------------------------------------------------------------
// Consultas
// ---------------------------------------------------------------------------

// GetWallet devolve a carteira atual.
func (s *Service) GetWallet(ctx context.Context, id string) (*wallet.Wallet, error) {
	if !wagering.IsUUID(id) {
		return nil, ErrWalletNotFound
	}
	w, err := scanWallet(s.db.QueryRowContext(ctx, `SELECT `+walletColumns+` FROM wallets WHERE id=$1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrWalletNotFound
	}
	return w, err
}

// GetTransaction busca por id interno.
func (s *Service) GetTransaction(ctx context.Context, id string) (*wagering.Transaction, error) {
	if !wagering.IsUUID(id) {
		return nil, ErrTransactionNotFound
	}
	t, err := scanTx(s.db.QueryRowContext(ctx, `SELECT `+txColumns+` FROM wager_transactions WHERE id=$1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTransactionNotFound
	}
	return t, err
}

// GetProviderTransaction busca por (providerId, externalTransactionId).
func (s *Service) GetProviderTransaction(ctx context.Context, providerID, externalID string) (*wagering.Transaction, error) {
	t, err := scanTx(s.db.QueryRowContext(ctx, `
		SELECT `+txColumns+` FROM wager_transactions
		 WHERE origin='EXTERNAL' AND provider_id=$1 AND external_transaction_id=$2`, providerID, externalID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTransactionNotFound
	}
	return t, err
}

// LedgerRow é um lançamento com sua posição (seq) para paginação por cursor.
type LedgerRow struct {
	Seq   int64
	Entry wallet.LedgerEntry
}

// LedgerPage devolve até limit lançamentos com seq > afterSeq, em ordem estável (seq crescente).
func (s *Service) LedgerPage(ctx context.Context, walletID string, afterSeq int64, limit int) ([]LedgerRow, error) {
	if !wagering.IsUUID(walletID) {
		return nil, ErrWalletNotFound
	}
	if _, err := s.GetWallet(ctx, walletID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT seq, id, transaction_id, direction, amount, currency, balance_before, balance_after, created_at
		  FROM wallet_ledger_entries
		 WHERE wallet_id=$1 AND seq > $2
		 ORDER BY seq LIMIT $3`, walletID, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LedgerRow
	for rows.Next() {
		var (
			seq, amount, before, after int64
			id, txID, dir, currency    string
			created                    time.Time
		)
		if err := rows.Scan(&seq, &id, &txID, &dir, &amount, &currency, &before, &after, &created); err != nil {
			return nil, err
		}
		cur := currencyOf(currency)
		a, _ := money.FromCents(amount, cur)
		b, _ := money.FromCents(before, cur)
		f, _ := money.FromCents(after, cur)
		e, err := wallet.RehydrateLedgerEntry(id, walletID, txID,
			wallet.Movement{Direction: wallet.Direction(dir), Amount: a, Before: b, After: f}, created)
		if err != nil {
			return nil, fmt.Errorf("ledger entry %s is inconsistent: %w", id, err)
		}
		out = append(out, LedgerRow{Seq: seq, Entry: e})
	}
	return out, rows.Err()
}

// Reconciliation compara o saldo armazenado com o reconstruído a partir do ledger.
type Reconciliation struct {
	WalletID       string
	Stored         money.Money
	Calculated     money.Money
	Difference     money.Money // armazenado - calculado
	Consistent     bool
	CheckedEntries int64
}

// Reconcile lê saldo e ledger no mesmo snapshot (REPEATABLE READ, somente leitura) e
// reporta divergências em log. Nunca altera o saldo.
func (s *Service) Reconcile(ctx context.Context, walletID string) (Reconciliation, error) {
	if !wagering.IsUUID(walletID) {
		return Reconciliation{}, ErrWalletNotFound
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return Reconciliation{}, err
	}
	defer tx.Rollback()

	w, err := scanWallet(tx.QueryRowContext(ctx, `SELECT `+walletColumns+` FROM wallets WHERE id=$1`, walletID))
	if errors.Is(err, sql.ErrNoRows) {
		return Reconciliation{}, ErrWalletNotFound
	}
	if err != nil {
		return Reconciliation{}, err
	}
	var sum, n int64
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(CASE direction WHEN 'CREDIT' THEN amount ELSE -amount END), 0)::bigint, COUNT(*)
		  FROM wallet_ledger_entries WHERE wallet_id=$1`, walletID).Scan(&sum, &n); err != nil {
		return Reconciliation{}, err
	}
	calc, err := money.FromCents(sum, w.Currency())
	if err != nil {
		return Reconciliation{}, err
	}
	diff, err := w.Balance().Sub(calc)
	if err != nil {
		return Reconciliation{}, err
	}
	r := Reconciliation{WalletID: walletID, Stored: w.Balance(), Calculated: calc, Difference: diff,
		Consistent: diff.IsZero(), CheckedEntries: n}
	if !r.Consistent {
		s.obs.ReconciliationDivergence()
		slog.Error("divergência de reconciliação", "walletId", walletID, "stored", r.Stored.Amount(), "calculated", r.Calculated.Amount(), "difference", diff.Amount())
	}
	return r, nil
}
