//go:build localstack

package inbound

// Testes contra um SQS REAL (LocalStack), sem filas em memória: o mesmo consumidor e o mesmo
// publicador da aplicação, usando o adaptador do SDK da AWS, PostgreSQL real e filas FIFO com
// DLQ e redrive provisionadas pelo teste.
//
//	docker compose up -d postgres localstack
//	TEST_DATABASE_URL=...  go test -tags localstack -count=1 -v ./internal/inbound/
//
// Variável opcional: SQS_TEST_ENDPOINT (padrão http://localhost:4566).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
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

type lsCounts struct {
	mu sync.Mutex
	m  map[string]int
}

func (c *lsCounts) SQSResult(r string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]int{}
	}
	c.m[r]++
}
func (c *lsCounts) get(r string) int { c.mu.Lock(); defer c.mu.Unlock(); return c.m[r] }

type lsEnv struct {
	t        *testing.T
	db       *sql.DB
	svc      *app.Service
	cmds     *messaging.SQS
	dlq      *messaging.SQS
	events   *messaging.SQS
	obs      *lsCounts
	walletID string
	player   string
}

// newLSEnv provisiona filas com nomes únicos (execuções não se misturam) e uma carteira.
func newLSEnv(t *testing.T, maxReceive int, balance string) *lsEnv {
	t.Helper()
	if os.Getenv("AWS_ACCESS_KEY_ID") == "" {
		t.Setenv("AWS_ACCESS_KEY_ID", "test")
		t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	}
	endpoint := os.Getenv("SQS_TEST_ENDPOINT")
	if endpoint == "" {
		endpoint = "http://localhost:4566"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := messaging.NewSQSClient(ctx, messaging.SQSConfig{Endpoint: endpoint, Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	id := strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	cmds, dlq, err := messaging.EnsureFIFOWithDLQ(ctx, client, "t"+id+"-cmd.fifo", "t"+id+"-cmd-dlq.fifo", 10*time.Second, maxReceive)
	if err != nil {
		t.Fatalf("não consegui criar filas em %s (o LocalStack está no ar? `docker compose up -d localstack`): %v", endpoint, err)
	}
	events, _, err := messaging.EnsureFIFOWithDLQ(ctx, client, "t"+id+"-evt.fifo", "t"+id+"-evt-dlq.fifo", 10*time.Second, 5)
	if err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("pgx", testutil.NewDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	svc := app.NewService(db, app.Config{})
	m, _ := money.Parse(balance, "BRL")
	player := uuid.NewString()
	w, err := svc.CreateWallet(context.Background(), player, m, "setup")
	if err != nil {
		t.Fatal(err)
	}
	return &lsEnv{t: t, db: db, svc: svc, cmds: cmds, dlq: dlq, events: events, obs: &lsCounts{}, walletID: w.ID(), player: player}
}

func (e *lsEnv) msg(kind, amount string, n int) CommandMessage {
	ext := fmt.Sprintf("ext-%s-%d", e.walletID[:8], n)
	return CommandMessage{
		ProviderID: "provider-a", IdempotencyKey: "key-" + ext, ExternalTransactionID: ext,
		PlayerID: e.player, WalletID: e.walletID, RoundID: "round-1", GameID: "game-1", Kind: kind,
		Money: wagering.MoneyData{Amount: amount, Currency: "BRL"}, CorrelationID: "corr-" + ext,
	}
}

func (e *lsEnv) send(m CommandMessage, dedupSuffix string) {
	e.t.Helper()
	err := e.cmds.Send(context.Background(), messaging.SendInput{
		Body: Encode(m), GroupID: m.WalletID, DeduplicationID: m.IdempotencyKey + dedupSuffix})
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *lsEnv) startConsumers(p Processor, instances int) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < instances; i++ {
		c := New(e.cmds, e.dlq, p, Config{Workers: 4, Wait: time.Second, BaseDelay: time.Second, MaxDelay: 2 * time.Second}, e.obs)
		wg.Add(1)
		go func() { defer wg.Done(); c.Run(ctx) }()
	}
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			done := make(chan struct{})
			go func() { wg.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				e.t.Error("consumidores não encerraram em 15s")
			}
		})
	}
	e.t.Cleanup(stop)
	return stop
}

func lsEventually(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("tempo esgotado esperando: %s", what)
}

func (e *lsEnv) count(q string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(q, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *lsEnv) balance() string {
	var c int64
	if err := e.db.QueryRow(`SELECT balance FROM wallets WHERE id=$1`, e.walletID).Scan(&c); err != nil {
		e.t.Fatal(err)
	}
	return fmt.Sprintf("%d.%02d", c/100, c%100)
}

// drained espera a fila ficar sem mensagens visíveis, em voo ou em atraso (duas leituras
// seguidas, porque as contagens do SQS são aproximadas).
func lsDrained(t *testing.T, q *messaging.SQS) {
	t.Helper()
	ok := 0
	lsEventually(t, 60*time.Second, "fila esvaziar", func() bool {
		v, f, d, err := q.Approximate(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if v+f+d == 0 {
			ok++
		} else {
			ok = 0
		}
		return ok >= 2
	})
}

// receiveAll lê (e apaga) até want mensagens de q dentro do prazo.
func lsReceive(t *testing.T, q *messaging.SQS, want int, within time.Duration) []messaging.Message {
	t.Helper()
	var got []messaging.Message
	end := time.Now().Add(within)
	for len(got) < want && time.Now().Before(end) {
		msgs, err := q.Receive(context.Background(), 10, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range msgs {
			got = append(got, m)
			_ = q.Delete(context.Background(), m.ReceiptHandle)
		}
	}
	return got
}

func TestSQSDuplicatesOrderAndNoOverdraft(t *testing.T) {
	e := newLSEnv(t, 5, "100.00")
	for i := 0; i < 20; i++ {
		e.send(e.msg("BET", "10.00", i), "")
	}
	// reenvios do mesmo comando, fora da deduplicação da fila (ids de deduplicação diferentes)
	e.send(e.msg("BET", "10.00", 0), "-dup-a")
	e.send(e.msg("BET", "10.00", 5), "-dup-b")
	e.startConsumers(e.svc, 2) // duas instâncias disputando a mesma fila

	lsEventually(t, 90*time.Second, "20 apostas registradas", func() bool {
		return e.count(`SELECT COUNT(*) FROM wager_transactions WHERE wallet_id=$1 AND kind='BET'`, e.walletID) == 20 &&
			e.count(`SELECT COUNT(*) FROM inbox_messages WHERE completed_at IS NOT NULL`) == 20
	})
	lsDrained(t, e.cmds)

	if got := e.balance(); got != "0.00" {
		t.Errorf("saldo = %s, esperado 0.00", got)
	}
	if n := e.count(`SELECT COUNT(*) FROM wager_transactions WHERE wallet_id=$1 AND kind='BET' AND status='PROCESSED'`, e.walletID); n != 10 {
		t.Errorf("processadas = %d, esperado 10", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM wager_transactions WHERE wallet_id=$1 AND status='REJECTED' AND failure_code='INSUFFICIENT_FUNDS'`, e.walletID); n != 10 {
		t.Errorf("rejeitadas = %d, esperado 10", n)
	}
	// ordem por carteira (grupo FIFO): só as 10 primeiras mensagens enviadas foram aceitas
	rows, err := e.db.Query(`SELECT external_transaction_id FROM wager_transactions WHERE wallet_id=$1 AND status='PROCESSED' AND kind='BET'`, e.walletID)
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
			t.Errorf("%s foi aceita antes de mensagens mais antigas da mesma carteira", ext)
		}
	}
	if d := e.obs.get("duplicate"); d < 2 {
		t.Errorf("duplicate = %d, esperado pelo menos 2 reentregas reconhecidas pela inbox", d)
	}
}

func TestSQSPoisonMessagesReachTheRealDLQ(t *testing.T) {
	e := newLSEnv(t, 5, "100.00")
	send := func(body, group, dedup string) {
		if err := e.cmds.Send(context.Background(), messaging.SendInput{Body: body, GroupID: group, DeduplicationID: dedup}); err != nil {
			t.Fatal(err)
		}
	}
	send("{not json", "poison-1", "p1")
	neg := e.msg("BET", "-5.00", 1)
	send(Encode(neg), "poison-2", "p2")
	e.send(e.msg("BET", "10.00", 9), "") // a mensagem boa segue normalmente
	e.startConsumers(e.svc, 1)

	got := lsReceive(t, e.dlq, 2, 60*time.Second)
	if len(got) != 2 {
		t.Fatalf("DLQ recebeu %d mensagens, esperado 2", len(got))
	}
	lsEventually(t, 30*time.Second, "mensagem boa processada", func() bool { return e.balance() == "90.00" })
	lsDrained(t, e.cmds)
	if e.obs.get("dead_lettered") != 2 {
		t.Errorf("dead_lettered = %d", e.obs.get("dead_lettered"))
	}
}

type lsFlaky struct {
	svc       *app.Service
	failTimes int32
	calls     int32
}

func (p *lsFlaky) ProcessWith(ctx context.Context, cmd wagering.Command, h app.Hooks) (app.Result, error) {
	after := h.After
	h.After = func(ctx context.Context, tx *sql.Tx, r app.Result) error {
		if atomic.AddInt32(&p.calls, 1) <= p.failTimes {
			return errors.New("conexão perdida antes do commit")
		}
		return after(ctx, tx, r)
	}
	return p.svc.ProcessWith(ctx, cmd, h)
}

func TestSQSFailureBeforeCommitIsRetriedAndAppliedOnce(t *testing.T) {
	e := newLSEnv(t, 5, "100.00")
	e.send(e.msg("BET", "30.00", 1), "")
	e.startConsumers(&lsFlaky{svc: e.svc, failTimes: 2}, 1)

	lsEventually(t, 90*time.Second, "aposta aplicada após as retentativas", func() bool { return e.balance() == "70.00" })
	lsDrained(t, e.cmds)
	if n := e.count(`SELECT COUNT(*) FROM wallet_ledger_entries WHERE wallet_id=$1 AND direction='DEBIT'`, e.walletID); n != 1 {
		t.Errorf("débitos = %d, esperado 1", n)
	}
	if e.obs.get("retried") != 2 {
		t.Errorf("retried = %d, esperado 2", e.obs.get("retried"))
	}
	if v, f, d, _ := e.dlq.Approximate(context.Background()); v+f+d != 0 {
		t.Errorf("a DLQ deveria estar vazia")
	}
}

func TestSQSRedriveMovesExhaustedMessageToDLQ(t *testing.T) {
	e := newLSEnv(t, 2, "100.00") // a fila move para a DLQ após 2 entregas
	e.send(e.msg("BET", "30.00", 1), "")
	e.startConsumers(&lsFlaky{svc: e.svc, failTimes: 1 << 30}, 1)

	got := lsReceive(t, e.dlq, 1, 90*time.Second)
	if len(got) != 1 {
		t.Fatalf("a DLQ deveria receber a mensagem por redrive; recebeu %d", len(got))
	}
	if b := e.balance(); b != "100.00" {
		t.Errorf("saldo = %s, esperado 100.00 (nada aplicado)", b)
	}
	if n := e.count(`SELECT COUNT(*) FROM inbox_messages`); n != 0 {
		t.Errorf("inbox deveria estar vazia, tem %d", n)
	}
}

func TestSQSOutboxPublishesToRealEventsQueueWithDeduplication(t *testing.T) {
	e := newLSEnv(t, 5, "100.00") // a abertura da carteira já gerou 2 eventos
	m := e.msg("BET", "10.00", 1)
	cmd, err := Decode(Encode(m))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Process(context.Background(), cmd); err != nil { // +2 eventos
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	pub := outbox.New(e.db, e.events, outbox.Config{}, nil)
	go func() { pub.Run(ctx, 200*time.Millisecond); close(done) }()
	defer func() { cancel(); <-done }()

	got := lsReceive(t, e.events, 4, 60*time.Second)
	if len(got) != 4 {
		t.Fatalf("eventos recebidos = %d, esperado 4", len(got))
	}
	types := map[string]int{}
	var firstEnv wagering.Envelope
	for i, g := range got {
		var env wagering.Envelope
		if err := json.Unmarshal([]byte(g.Body), &env); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			firstEnv = env
		}
		types[env.EventType]++
	}
	if types[wagering.EventTransactionProcessed] != 2 || types[wagering.EventWalletBalanceChanged] != 2 {
		t.Errorf("tipos de evento: %v", types)
	}
	lsEventually(t, 15*time.Second, "outbox toda publicada", func() bool {
		return e.count(`SELECT COUNT(*) FROM outbox_events WHERE published_at IS NULL`) == 0
	})

	// Republicar o mesmo evento (mesmo eventId = MessageDeduplicationId) deve ser descartado pela fila.
	if err := e.events.Send(context.Background(), messaging.SendInput{
		Body: "{}", GroupID: firstEnv.AggregateID, DeduplicationID: firstEnv.EventID}); err != nil {
		t.Fatal(err)
	}
	if again := lsReceive(t, e.events, 1, 5*time.Second); len(again) != 0 {
		t.Errorf("a fila deveria descartar o reenvio com o mesmo eventId, mas entregou %d", len(again))
	}
}

// lsDeleteFailsOnce simula uma falha de Delete depois do commit,
// delegando Receive, Send e Release ao SQS real do LocalStack.
type lsDeleteFailsOnce struct {
	messaging.Queue
	failed atomic.Bool
}

func (q *lsDeleteFailsOnce) Delete(ctx context.Context, receiptHandle string) error {
	if q.failed.CompareAndSwap(false, true) {
		return errors.New("falha simulada no Delete após o commit")
	}
	return q.Queue.Delete(ctx, receiptHandle)
}

func TestSQSDeleteFailureAfterCommitIsHarmless(t *testing.T) {
	e := newLSEnv(t, 5, "100.00")

	// A mensagem será processada pelo SQS real do LocalStack.
	e.send(e.msg("BET", "25.00", 1), "")

	// A primeira tentativa de exclusão falha; a seguinte usa o SQS real.
	wrapped := &lsDeleteFailsOnce{Queue: e.cmds}

	ctx, cancel := context.WithCancel(context.Background())
	c := New(
		wrapped,
		e.dlq,
		e.svc,
		Config{
			Workers:   1,
			Wait:      time.Second,
			BaseDelay: time.Second,
			MaxDelay:  2 * time.Second,
		},
		e.obs,
	)

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(ctx)
	}()

	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("consumidor não encerrou em 15 segundos")
		}
	})

	// O SQS mantém a mensagem invisível até o VisibilityTimeout expirar.
	// Na reentrega, a Inbox deve reconhecê-la como duplicata.
	lsEventually(t, 60*time.Second, "reentrega reconhecida como duplicata", func() bool {
		return wrapped.failed.Load() &&
			e.obs.get("duplicate") == 1 &&
			e.count(
				`SELECT COUNT(*) FROM inbox_messages
				 WHERE consumer_name='wager-commands'
				   AND completed_at IS NOT NULL`,
			) == 1 &&
			e.balance() == "75.00"
	})

	// Confirma que a mensagem foi efetivamente removida do SQS após a reentrega.
	lsDrained(t, e.cmds)

	if got := e.balance(); got != "75.00" {
		t.Errorf("saldo = %s, esperado 75.00", got)
	}

	if n := e.count(
		`SELECT COUNT(*) FROM wager_transactions
		 WHERE wallet_id=$1 AND kind='BET' AND status='PROCESSED'`,
		e.walletID,
	); n != 1 {
		t.Errorf("apostas processadas = %d, esperado 1", n)
	}

	if n := e.count(
		`SELECT COUNT(*) FROM wallet_ledger_entries
		 WHERE wallet_id=$1 AND direction='DEBIT'`,
		e.walletID,
	); n != 1 {
		t.Errorf("débitos no ledger = %d, esperado 1", n)
	}

	if e.obs.get("acked") != 1 || e.obs.get("duplicate") != 1 {
		t.Errorf(
			"acked=%d duplicate=%d, esperado 1 e 1",
			e.obs.get("acked"),
			e.obs.get("duplicate"),
		)
	}

	if v, f, d, err := e.dlq.Approximate(context.Background()); err != nil {
		t.Fatal(err)
	} else if v+f+d != 0 {
		t.Errorf("DLQ deveria estar vazia; visíveis=%d em voo=%d atrasadas=%d", v, f, d)
	}
}
