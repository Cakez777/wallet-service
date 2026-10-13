//go:build integration

package inbound

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cakez777/wallet-service/internal/app"
	"github.com/Cakez777/wallet-service/internal/messaging"
	"github.com/Cakez777/wallet-service/internal/money"
	"github.com/Cakez777/wallet-service/internal/outbox"
	"github.com/Cakez777/wallet-service/internal/testutil"
	"github.com/Cakez777/wallet-service/internal/wagering"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

type counts struct {
	mu sync.Mutex
	m  map[string]int
}

func (c *counts) SQSResult(r string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]int{}
	}
	c.m[r]++
}

func (c *counts) get(r string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[r]
}

type fixture struct {
	t     *testing.T
	db    *sql.DB
	svc   *app.Service
	q     *messaging.Memory
	dlq   *messaging.Memory
	obs   *counts
	walID string
}

func newFixture(t *testing.T, balance string, maxReceive int) *fixture {
	t.Helper()
	dsn := testutil.NewDatabase(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	svc := app.NewService(db, app.Config{})
	dlq := messaging.NewMemory(time.Minute, 0, nil)
	q := messaging.NewMemory(300*time.Millisecond, maxReceive, dlq)

	m, _ := money.Parse(balance, "BRL")
	w, err := svc.CreateWallet(context.Background(), uuid.NewString(), m, "setup")
	if err != nil {
		t.Fatal(err)
	}

	return &fixture{
		t:     t,
		db:    db,
		svc:   svc,
		q:     q,
		dlq:   dlq,
		obs:   &counts{},
		walID: w.ID(),
	}
}

func (f *fixture) msg(kind, amount string, n int) CommandMessage {
	ext := fmt.Sprintf("ext-%s-%d", f.walID[:8], n)
	return CommandMessage{
		ProviderID:            "provider-a",
		IdempotencyKey:        "key-" + ext,
		ExternalTransactionID: ext,
		PlayerID:              f.playerID(),
		WalletID:              f.walID,
		RoundID:               "round-1",
		GameID:                "game-1",
		Kind:                  kind,
		Money: wagering.MoneyData{
			Amount:   amount,
			Currency: "BRL",
		},
		CorrelationID: "corr-" + ext,
	}
}

func (f *fixture) playerID() string {
	var p string
	if err := f.db.QueryRow(
		`SELECT player_id FROM wallets WHERE id=$1`,
		f.walID,
	).Scan(&p); err != nil {
		f.t.Fatal(err)
	}
	return p
}

// publish envia como um produtor FIFO faria: grupo = carteira, deduplicação = chave de idempotência
// (mais um sufixo opcional para simular reenvios fora da janela de 5 minutos da fila).
func (f *fixture) publish(m CommandMessage, dedupSuffix string) {
	f.t.Helper()
	err := f.q.Send(context.Background(), messaging.SendInput{
		Body:            Encode(m),
		GroupID:         m.WalletID,
		DeduplicationID: m.IdempotencyKey + dedupSuffix,
	})
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) start(p Processor, instances int) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	for i := 0; i < instances; i++ {
		c := New(
			f.q,
			f.dlq,
			p,
			Config{
				Workers:   4,
				Wait:      20 * time.Millisecond,
				BaseDelay: 20 * time.Millisecond,
				MaxDelay:  100 * time.Millisecond,
			},
			f.obs,
		)

		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Run(ctx)
		}()
	}

	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()

		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			f.t.Error("consumidor não encerrou em 5s após o cancelamento")
		}
	}

	f.t.Cleanup(stop)
	return stop
}

func eventually(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	end := time.Now().Add(d)

	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("tempo esgotado esperando: %s", what)
}

func (f *fixture) balance() string {
	var c int64
	if err := f.db.QueryRow(
		`SELECT balance FROM wallets WHERE id=$1`,
		f.walID,
	).Scan(&c); err != nil {
		f.t.Fatal(err)
	}

	return fmt.Sprintf("%d.%02d", c/100, c%100)
}

func (f *fixture) count(query string, args ...any) int {
	var n int
	if err := f.db.QueryRow(query, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func TestDecodeOfficialWagerEnvelope(t *testing.T) {
	f := newFixture(t, "100.00", 5)

	m := f.msg("BET", "10.00", 1)
	m.CorrelationID = "corr-official-envelope"

	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}

	body := fmt.Sprintf(
		`{"messageId":"msg-official-456","type":"WagerTransactionRequested","occurredAt":"2026-09-18T12:00:00.000Z","data":%s}`,
		data,
	)

	cmd, messageID, err := decodeEnvelope(body)
	if err != nil {
		t.Fatalf("decodeEnvelope retornou erro: %v", err)
	}

	if messageID != "msg-official-456" {
		t.Errorf("messageID = %q; esperado %q", messageID, "msg-official-456")
	}

	if cmd.ProviderID != m.ProviderID {
		t.Errorf("ProviderID = %q; esperado %q", cmd.ProviderID, m.ProviderID)
	}

	if cmd.ExternalTransactionID != m.ExternalTransactionID {
		t.Errorf(
			"ExternalTransactionID = %q; esperado %q",
			cmd.ExternalTransactionID,
			m.ExternalTransactionID,
		)
	}

	if cmd.Money.Amount() != "10.00" {
		t.Errorf("Money.Amount = %q, esperado %q", cmd.Money.Amount(), "10.00")
	}
}

func TestDuplicateDeliveriesAreAppliedOnce(t *testing.T) {
	f := newFixture(t, "100.00", 5)
	m := f.msg("BET", "50.00", 1)

	// Todas as entregas representam a mesma mensagem de negócio.
	m.MessageID = "official-message-duplicate-001"

	for i := 0; i < 5; i++ {
		// Os sufixos permitem publicar cinco entregas na fila,
		// contornando a deduplicação da própria fila.
		f.publish(m, fmt.Sprint("-dup", i))
	}

	f.start(f.svc.WithObserver(nil), 2)

	eventually(t, 10*time.Second, "fila esvaziar", func() bool {
		return f.q.Depth() == 0
	})

	if got := f.balance(); got != "50.00" {
		t.Errorf("saldo = %s, esperado 50.00 (um único débito)", got)
	}

	if n := f.count(
		`SELECT COUNT(*) FROM wallet_ledger_entries WHERE wallet_id=$1 AND direction='DEBIT'`,
		f.walID,
	); n != 1 {
		t.Errorf("lançamentos de débito = %d, esperado 1", n)
	}

	if n := f.count(
		`SELECT COUNT(*) FROM inbox_messages WHERE completed_at IS NOT NULL`,
	); n != 1 {
		t.Errorf("linhas na inbox = %d, esperado 1", n)
	}

	if f.obs.get("acked") != 1 || f.obs.get("duplicate") != 4 {
		t.Errorf(
			"acked=%d duplicate=%d, esperado 1 e 4",
			f.obs.get("acked"),
			f.obs.get("duplicate"),
		)
	}
}

func TestTwoInstancesConsumeSameWalletInOrderWithoutOverdraft(t *testing.T) {
	f := newFixture(t, "100.00", 5)

	for i := 0; i < 20; i++ {
		f.publish(f.msg("BET", "10.00", i), "")
	}

	f.start(f.svc, 2)
	eventually(t, 20*time.Second, "fila esvaziar", func() bool {
		return f.q.Depth() == 0
	})

	if got := f.balance(); got != "0.00" {
		t.Errorf("saldo = %s, esperado 0.00", got)
	}

	if n := f.count(
		`SELECT COUNT(*) FROM wager_transactions WHERE wallet_id=$1 AND status='PROCESSED' AND kind='BET'`,
		f.walID,
	); n != 10 {
		t.Errorf("processadas = %d, esperado 10", n)
	}

	if n := f.count(
		`SELECT COUNT(*) FROM wager_transactions WHERE wallet_id=$1 AND status='REJECTED' AND failure_code='INSUFFICIENT_FUNDS'`,
		f.walID,
	); n != 10 {
		t.Errorf("rejeitadas por saldo = %d, esperado 10", n)
	}

	// Ordem por carteira: as 10 primeiras mensagens enviadas são as que passam.
	rows, err := f.db.Query(
		`SELECT external_transaction_id FROM wager_transactions WHERE wallet_id=$1 AND status='PROCESSED' AND kind='BET'`,
		f.walID,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	for rows.Next() {
		var ext string
		_ = rows.Scan(&ext)

		var n int
		fmt.Sscanf(ext[len("ext-")+8+1:], "%d", &n)

		if n >= 10 {
			t.Errorf("%s foi processada antes de mensagens mais antigas da mesma carteira", ext)
		}
	}
}

func TestPoisonMessagesGoStraightToDLQ(t *testing.T) {
	f := newFixture(t, "100.00", 5)

	grp := func(body, id string) {
		_ = f.q.Send(context.Background(), messaging.SendInput{
			Body:            body,
			GroupID:         "poison-" + id,
			DeduplicationID: id,
		})
	}

	grp("{not json", "p1")

	bad := f.msg("BET", "-5.00", 1)
	grp(Encode(bad), "p2")

	bad2 := f.msg("BET", "5.00", 2)
	bad2.Kind = "TRANSFER"
	grp(Encode(bad2), "p3")

	f.publish(f.msg("BET", "10.00", 9), "")

	f.start(f.svc, 1)
	eventually(t, 10*time.Second, "fila esvaziar", func() bool {
		return f.q.Depth() == 0
	})

	if f.dlq.Depth() != 3 {
		t.Errorf("DLQ = %d, esperado 3 mensagens veneno", f.dlq.Depth())
	}

	if got := f.balance(); got != "90.00" {
		t.Errorf("saldo = %s, esperado 90.00 (só a mensagem boa)", got)
	}

	if f.obs.get("dead_lettered") != 3 {
		t.Errorf("dead_lettered = %d", f.obs.get("dead_lettered"))
	}
}

func TestSameKeyDifferentPayloadIsDeadLettered(t *testing.T) {
	f := newFixture(t, "100.00", 5)

	a := f.msg("BET", "10.00", 1)
	b := a
	b.Money.Amount = "99.00"

	f.publish(a, "")
	f.publish(b, "-b")

	f.start(f.svc, 1)
	eventually(t, 10*time.Second, "fila esvaziar", func() bool {
		return f.q.Depth() == 0
	})

	if f.dlq.Depth() != 1 || f.balance() != "90.00" {
		t.Errorf("dlq=%d saldo=%s; esperado 1 e 90.00", f.dlq.Depth(), f.balance())
	}
}

// flaky derruba a transação (depois de gravar saldo e inbox, antes do commit) nas primeiras
// failTimes tentativas, simulando perda de conexão no pior momento.
type flaky struct {
	svc       *app.Service
	failTimes int32
	calls     int32
}

func (p *flaky) ProcessWith(ctx context.Context, cmd wagering.Command, h app.Hooks) (app.Result, error) {
	after := h.After
	h.After = func(ctx context.Context, tx *sql.Tx, r app.Result) error {
		if atomic.AddInt32(&p.calls, 1) <= p.failTimes {
			return errors.New("conexão perdida antes do commit")
		}
		return after(ctx, tx, r)
	}

	return p.svc.ProcessWith(ctx, cmd, h)
}

func TestFailureBeforeCommitRollsBackEverythingAndRetries(t *testing.T) {
	f := newFixture(t, "100.00", 5)
	f.publish(f.msg("BET", "30.00", 1), "")

	p := &flaky{svc: f.svc, failTimes: 2}
	f.start(p, 1)

	eventually(t, 10*time.Second, "fila esvaziar", func() bool {
		return f.q.Depth() == 0
	})

	if got := f.balance(); got != "70.00" {
		t.Errorf("saldo = %s, esperado 70.00 (débito único apesar de 2 falhas)", got)
	}

	if n := f.count(
		`SELECT COUNT(*) FROM wallet_ledger_entries WHERE wallet_id=$1 AND direction='DEBIT'`,
		f.walID,
	); n != 1 {
		t.Errorf("débitos = %d, esperado 1", n)
	}

	if f.obs.get("retried") != 2 || f.obs.get("acked") != 1 {
		t.Errorf("retried=%d acked=%d, esperado 2 e 1", f.obs.get("retried"), f.obs.get("acked"))
	}

	if f.dlq.Depth() != 0 {
		t.Errorf("DLQ = %d, esperado 0", f.dlq.Depth())
	}
}

func TestExhaustedRetriesEndInDLQWithoutSideEffects(t *testing.T) {
	f := newFixture(t, "100.00", 3)
	f.publish(f.msg("BET", "30.00", 1), "")

	f.start(&flaky{svc: f.svc, failTimes: 1 << 30}, 1)

	eventually(t, 10*time.Second, "mensagem chegar à DLQ", func() bool {
		return f.dlq.Depth() == 1
	})

	if got := f.balance(); got != "100.00" {
		t.Errorf("saldo = %s, esperado 100.00 (nada aplicado)", got)
	}

	if n := f.count(`SELECT COUNT(*) FROM inbox_messages`); n != 0 {
		t.Errorf("inbox deveria estar vazia, tem %d", n)
	}
}

type deleteFailsOnce struct {
	messaging.Queue
	failed atomic.Bool
}

func (q *deleteFailsOnce) Delete(ctx context.Context, r string) error {
	if q.failed.CompareAndSwap(false, true) {
		return errors.New("falha no delete após o commit")
	}
	return q.Queue.Delete(ctx, r)
}

func TestDeleteFailureAfterCommitIsHarmless(t *testing.T) {
	f := newFixture(t, "100.00", 5)
	f.publish(f.msg("BET", "25.00", 1), "")

	wrapped := &deleteFailsOnce{Queue: f.q}
	ctx, cancel := context.WithCancel(context.Background())

	c := New(
		wrapped,
		f.dlq,
		f.svc,
		Config{
			Workers:   1,
			Wait:      20 * time.Millisecond,
			BaseDelay: 20 * time.Millisecond,
		},
		f.obs,
	)

	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()

	defer func() {
		cancel()
		<-done
	}()

	eventually(t, 10*time.Second, "fila esvaziar", func() bool {
		return f.q.Depth() == 0
	})

	if got := f.balance(); got != "75.00" {
		t.Errorf("saldo = %s, esperado 75.00 (a reentrega não pode debitar de novo)", got)
	}

	if f.obs.get("acked") != 1 || f.obs.get("duplicate") != 1 {
		t.Errorf("acked=%d duplicate=%d, esperado 1 e 1", f.obs.get("acked"), f.obs.get("duplicate"))
	}
}

func TestGracefulStopFinishesInFlightWork(t *testing.T) {
	f := newFixture(t, "1000.00", 5)

	for i := 0; i < 30; i++ {
		f.publish(f.msg("BET", "1.00", i), "")
	}

	stop := f.start(f.svc, 1)
	time.Sleep(100 * time.Millisecond)
	stop()

	var processed, inLedger int

	processed = f.count(
		`SELECT COUNT(*) FROM wager_transactions WHERE wallet_id=$1 AND kind='BET'`,
		f.walID,
	)
	inLedger = f.count(
		`SELECT COUNT(*) FROM wallet_ledger_entries WHERE wallet_id=$1 AND direction='DEBIT'`,
		f.walID,
	)

	if processed != inLedger {
		t.Errorf("transações=%d lançamentos=%d: estado inconsistente após o encerramento", processed, inLedger)
	}

	r, err := f.svc.Reconcile(context.Background(), f.walID)
	if err != nil || !r.Consistent {
		t.Errorf("reconciliação após o encerramento: %+v err=%v", r, err)
	}
}

// Ponta a ponta sem SQS real: comando -> consumidor -> PostgreSQL (saldo + outbox na mesma
// transação) -> publicador -> fila de eventos.
func TestEndToEndCommandToPublishedEvents(t *testing.T) {
	f := newFixture(t, "100.00", 5)
	f.publish(f.msg("BET", "40.00", 1), "")

	f.start(f.svc, 1)
	eventually(t, 10*time.Second, "fila de comandos esvaziar", func() bool {
		return f.q.Depth() == 0
	})

	events := messaging.NewMemory(time.Minute, 0, nil)
	pub := outbox.New(f.db, events, outbox.Config{}, nil)

	end := time.Now().Add(10 * time.Second)
	for events.Depth() < 3 && time.Now().Before(end) {
		if _, err := pub.PublishBatch(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	types := map[string]int{}
	corr := ""

	for _, b := range events.Bodies() {
		var e wagering.Envelope
		if err := json.Unmarshal([]byte(b), &e); err != nil {
			t.Fatal(err)
		}

		types[e.EventType]++

		if e.CorrelationID == "corr-ext-"+f.walID[:8]+"-1" {
			corr = e.CorrelationID
		}
	}

	if types[wagering.EventTransactionProcessed] < 1 ||
		types[wagering.EventWalletBalanceChanged] < 1 {
		t.Errorf("eventos publicados: %v", types)
	}

	if corr == "" {
		t.Errorf("o correlationId do comando deveria acompanhar os eventos: %v", events.Bodies())
	}
}

func TestMissingCorrelationIDFallsBackToMessageID(t *testing.T) {
	f := newFixture(t, "100.00", 5)

	m := f.msg("BET", "10.00", 1)
	m.CorrelationID = ""

	f.publish(m, "")
	f.start(f.svc, 1)

	eventually(t, 10*time.Second, "fila esvaziar", func() bool {
		return f.q.Depth() == 0
	})

	var corr string
	if err := f.db.QueryRow(
		`SELECT correlation_id FROM wager_transactions WHERE wallet_id=$1 AND kind='BET'`,
		f.walID,
	).Scan(&corr); err != nil {
		t.Fatal(err)
	}

	if corr == "" {
		t.Error("correlationId deveria ter um valor de fallback (id da mensagem)")
	}

	var inEvents int
	if err := f.db.QueryRow(
		`SELECT COUNT(*) FROM outbox_events WHERE payload->>'correlationId' = $1`,
		corr,
	).Scan(&inEvents); err != nil {
		t.Fatal(err)
	}

	if inEvents < 1 {
		t.Errorf("os eventos deveriam carregar o correlationId %q", corr)
	}
}
