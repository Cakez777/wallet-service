//go:build integration

package app

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cakez777/wallet-service/internal/money"
	"github.com/Cakez777/wallet-service/internal/wagering"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Estes testes rodam contra um PostgreSQL real. Defina TEST_DATABASE_URL apontando para
// qualquer banco do servidor (ex.: postgres://wallet:wallet@localhost:5432/wallet?sslmode=disable);
// cada execução cria e apaga um banco próprio, aplicando as migrations "up".

type env struct {
	t     *testing.T
	admin *sql.DB
	dsn   string // DSN do banco temporário
	pools []*sql.DB
}

func newEnv(t *testing.T) *env {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Fatal("TEST_DATABASE_URL não definida: os testes de integração exigem um PostgreSQL real")
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	name := "wallet_it_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatalf("criando banco de teste: %v", err)
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	e := &env{t: t, admin: admin, dsn: u.String()}

	files, _ := filepath.Glob("../../db/migrations/*.up.sql")
	sort.Strings(files)
	if len(files) == 0 {
		t.Fatal("nenhuma migration encontrada em db/migrations")
	}
	db := e.pool()
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(b)); err != nil {
			t.Fatalf("migration %s: %v", f, err)
		}
	}
	t.Cleanup(func() {
		for _, p := range e.pools {
			p.Close()
		}
		_, _ = admin.Exec("DROP DATABASE " + name + " WITH (FORCE)")
		admin.Close()
	})
	return e
}

// pool abre um pool independente (conexões próprias), simulando uma instância separada.
func (e *env) pool() *sql.DB {
	db, err := sql.Open("pgx", e.dsn)
	if err != nil {
		e.t.Fatal(err)
	}
	db.SetMaxOpenConns(20)
	e.pools = append(e.pools, db)
	return db
}

// services devolve n instâncias do serviço, cada uma com seu próprio pool.
func (e *env) services(n int, cfg Config) []*Service {
	out := make([]*Service, n)
	for i := range out {
		out[i] = NewService(e.pool(), cfg)
	}
	return out
}

func (e *env) db() *sql.DB { return e.pools[0] }

func brl(t *testing.T, s string) money.Money {
	t.Helper()
	m, err := money.Parse(s, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

type fixture struct{ walletID, playerID string }

func (e *env) wallet(svc *Service, balance string) fixture {
	e.t.Helper()
	player := uuid.NewString()
	w, err := svc.CreateWallet(context.Background(), player, brl(e.t, balance), "")
	if err != nil {
		e.t.Fatalf("CreateWallet: %v", err)
	}
	return fixture{walletID: w.ID(), playerID: player}
}

func (f fixture) cmd(t *testing.T, kind wagering.Kind, ext, amount string) wagering.Command {
	t.Helper()
	provider := "provider-" + f.walletID[len(f.walletID)-8:] // um provedor por carteira de teste: ids externos são únicos por provedor
	return wagering.Command{
		ProviderID: provider, ExternalTransactionID: ext, IdempotencyKey: provider + ":" + ext,
		PlayerID: f.playerID, WalletID: f.walletID, RoundID: "round-1", GameID: "fortune-chimp",
		Kind: kind, Money: brl(t, amount), CorrelationID: "corr-" + ext,
	}
}

func (f fixture) ref(t *testing.T, kind wagering.Kind, ext, amount, refExt string) wagering.Command {
	c := f.cmd(t, kind, ext, amount)
	c.ReferenceExternalTransactionID = refExt
	return c
}

func (e *env) must(svc *Service, c wagering.Command) Result {
	e.t.Helper()
	r, err := svc.Process(context.Background(), c)
	if err != nil {
		e.t.Fatalf("Process(%s %s): %v", c.Kind, c.ExternalTransactionID, err)
	}
	return r
}

func (e *env) balance(walletID string) string {
	e.t.Helper()
	var cents int64
	if err := e.db().QueryRow(`SELECT balance FROM wallets WHERE id=$1`, walletID).Scan(&cents); err != nil {
		e.t.Fatal(err)
	}
	m, _ := money.FromCents(cents, "BRL")
	return m.Amount()
}

func (e *env) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db().QueryRow(query, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) ledgerCount(walletID, direction string) int {
	return e.count(`SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id=$1 AND direction=$2`, walletID, direction)
}

func (e *env) events(txID, eventType string) int {
	return e.count(`SELECT count(*) FROM outbox_events WHERE event_type=$1 AND payload->'data'->>'transactionId'=$2`, eventType, txID)
}

// assertInvariants: saldo armazenado = créditos - débitos do ledger, e reconciliação consistente.
func (e *env) assertInvariants(svc *Service, walletID string) {
	e.t.Helper()
	var stored, ledger int64
	if err := e.db().QueryRow(`SELECT balance FROM wallets WHERE id=$1`, walletID).Scan(&stored); err != nil {
		e.t.Fatal(err)
	}
	if err := e.db().QueryRow(`SELECT COALESCE(SUM(CASE direction WHEN 'CREDIT' THEN amount ELSE -amount END),0)::bigint
		FROM wallet_ledger_entries WHERE wallet_id=$1`, walletID).Scan(&ledger); err != nil {
		e.t.Fatal(err)
	}
	if stored != ledger || stored < 0 {
		e.t.Fatalf("carteira %s: saldo %d, ledger %d", walletID, stored, ledger)
	}
	r, err := svc.Reconcile(context.Background(), walletID)
	if err != nil || !r.Consistent || !r.Difference.IsZero() {
		e.t.Fatalf("reconciliação: %+v %v", r, err)
	}
}

func TestCreateWalletOpening(t *testing.T) {
	e := newEnv(t)
	svc := e.services(1, Config{})[0]
	ctx := context.Background()

	player := uuid.NewString()
	w, err := svc.CreateWallet(ctx, player, brl(t, "1000.00"), "corr-1")
	if err != nil {
		t.Fatal(err)
	}
	if w.Version() != 1 || w.Balance().Amount() != "1000.00" {
		t.Fatalf("carteira: v%d %s", w.Version(), w.Balance().Amount())
	}
	var openingID string
	if err := e.db().QueryRow(`SELECT id FROM wager_transactions WHERE wallet_id=$1 AND kind='OPENING' AND status='PROCESSED' AND origin='INTERNAL'`, w.ID()).Scan(&openingID); err != nil {
		t.Fatalf("OPENING não encontrada: %v", err)
	}
	if e.ledgerCount(w.ID(), "CREDIT") != 1 {
		t.Error("esperava 1 crédito de abertura no ledger")
	}
	if e.events(openingID, wagering.EventTransactionProcessed) != 1 || e.events(openingID, wagering.EventWalletBalanceChanged) != 1 {
		t.Error("esperava WagerTransactionProcessed e WalletBalanceChanged da abertura")
	}
	e.assertInvariants(svc, w.ID())

	if _, err := svc.CreateWallet(ctx, player, brl(t, "5.00"), ""); err != ErrWalletAlreadyExists {
		t.Errorf("mesma carteira (jogador+moeda) deveria dar conflito, veio %v", err)
	}
	usd, _ := money.Parse("0.00", "USD")
	other, err := svc.CreateWallet(ctx, player, usd, "")
	if err != nil {
		t.Fatalf("moeda diferente deve ser outra carteira: %v", err)
	}
	// saldo inicial zero: sem OPENING, sem ledger, sem eventos
	if e.count(`SELECT count(*) FROM wager_transactions WHERE wallet_id=$1`, other.ID()) != 0 ||
		e.count(`SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id=$1`, other.ID()) != 0 {
		t.Error("saldo zero não deve criar OPENING nem ledger")
	}
	if _, err := svc.CreateWallet(ctx, "nao-e-uuid", brl(t, "1.00"), ""); !wagering.IsValidationError(err) {
		t.Errorf("playerId inválido deveria ser ValidationError, veio %v", err)
	}
}

func TestBetWinLossFlow(t *testing.T) {
	e := newEnv(t)
	svc := e.services(1, Config{})[0]
	f := e.wallet(svc, "1000.00")

	bet := e.must(svc, f.cmd(t, wagering.KindBet, "bet-1", "25.00"))
	if bet.Replay || bet.Transaction.Status() != wagering.StatusProcessed || bet.Transaction.ResultBalance().Amount() != "975.00" {
		t.Fatalf("BET: %+v", bet)
	}
	win := e.must(svc, f.ref(t, wagering.KindWin, "win-1", "60.00", "bet-1"))
	if win.Transaction.ResultBalance().Amount() != "1035.00" {
		t.Fatalf("WIN: saldo %s", win.Transaction.ResultBalance().Amount())
	}
	var version int64
	_ = e.db().QueryRow(`SELECT version FROM wallets WHERE id=$1`, f.walletID).Scan(&version)

	loss := e.must(svc, f.cmd(t, wagering.KindLoss, "loss-1", "0.00"))
	if loss.Transaction.Status() != wagering.StatusProcessed || loss.Transaction.ResultBalance().Amount() != "1035.00" {
		t.Fatalf("LOSS: %+v", loss.Transaction.Status())
	}
	var after int64
	_ = e.db().QueryRow(`SELECT version FROM wallets WHERE id=$1`, f.walletID).Scan(&after)
	if after != version {
		t.Errorf("LOSS não deve alterar a versão: %d -> %d", version, after)
	}
	if e.count(`SELECT count(*) FROM wallet_ledger_entries WHERE transaction_id=$1`, loss.Transaction.ID()) != 0 {
		t.Error("LOSS não deve criar lançamento")
	}
	if e.events(loss.Transaction.ID(), wagering.EventTransactionProcessed) != 1 || e.events(loss.Transaction.ID(), wagering.EventWalletBalanceChanged) != 0 {
		t.Error("LOSS deve gerar Processed e não gerar WalletBalanceChanged")
	}
	if e.events(bet.Transaction.ID(), wagering.EventWalletBalanceChanged) != 1 {
		t.Error("BET deve gerar WalletBalanceChanged")
	}
	e.assertInvariants(svc, f.walletID)

	// WIN com referência para uma BET de outra rodada é rejeitado
	other := f.cmd(t, wagering.KindWin, "win-2", "1.00")
	other.RoundID = "round-2"
	other.ReferenceExternalTransactionID = "bet-1"
	r := e.must(svc, other)
	if r.Transaction.Status() != wagering.StatusRejected || r.Transaction.FailureCode() != wagering.FailReferenceMismatch {
		t.Errorf("WIN com rodada diferente: %s %s", r.Transaction.Status(), r.Transaction.FailureCode())
	}
}

func TestIdempotencyPersistsAndConflicts(t *testing.T) {
	e := newEnv(t)
	svcs := e.services(2, Config{})
	f := e.wallet(svcs[0], "100.00")

	first := e.must(svcs[0], f.cmd(t, wagering.KindBet, "bet-1", "30.00"))
	if first.Replay {
		t.Fatal("primeira entrega não é replay")
	}
	e.must(svcs[0], f.cmd(t, wagering.KindWin, "win-1", "10.00")) // saldo muda depois da BET

	// reenvio por OUTRA instância: replay com o saldo observado no processamento original
	again := e.must(svcs[1], f.cmd(t, wagering.KindBet, "bet-1", "30.00"))
	if !again.Replay || again.Transaction.ID() != first.Transaction.ID() || again.Transaction.ResultBalance().Amount() != "70.00" {
		t.Fatalf("replay: %+v saldo=%s", again.Replay, again.Transaction.ResultBalance().Amount())
	}
	if e.balance(f.walletID) != "80.00" {
		t.Errorf("replay alterou o saldo: %s", e.balance(f.walletID))
	}

	// mesma chave, conteúdo diferente
	diff := f.cmd(t, wagering.KindBet, "bet-1", "31.00")
	if _, err := svcs[1].Process(context.Background(), diff); err != ErrIdempotencyConflict {
		t.Errorf("mesma chave com conteúdo diferente: %v", err)
	}
	// mesma operação externa, outra chave
	dup := f.cmd(t, wagering.KindBet, "bet-1", "30.00")
	dup.IdempotencyKey = "chave-nova"
	if _, err := svcs[1].Process(context.Background(), dup); err != ErrExternalTransactionConflict {
		t.Errorf("mesma operação com outra chave: %v", err)
	}
	// correlationId e transporte não afetam a equivalência
	same := f.cmd(t, wagering.KindBet, "bet-1", "30.00")
	same.CorrelationID = "outra"
	if r := e.must(svcs[0], same); !r.Replay {
		t.Error("correlationId diferente ainda deve ser replay")
	}
	// carteira inexistente
	ghost := f.cmd(t, wagering.KindBet, "bet-9", "1.00")
	ghost.WalletID = uuid.NewString()
	if _, err := svcs[0].Process(context.Background(), ghost); err != ErrWalletNotFound {
		t.Errorf("carteira inexistente: %v", err)
	}
	e.assertInvariants(svcs[0], f.walletID)
}

func TestRejectionIsPersistedAndReplayed(t *testing.T) {
	e := newEnv(t)
	svcs := e.services(2, Config{})
	f := e.wallet(svcs[0], "10.00")

	c := f.cmd(t, wagering.KindBet, "bet-1", "50.00")
	r := e.must(svcs[0], c)
	if r.Transaction.Status() != wagering.StatusRejected || r.Transaction.FailureCode() != wagering.FailInsufficientFunds {
		t.Fatalf("esperava rejeição por saldo: %s %s", r.Transaction.Status(), r.Transaction.FailureCode())
	}
	if e.events(r.Transaction.ID(), wagering.EventTransactionRejected) != 1 {
		t.Error("rejeição deve gerar WagerTransactionRejected")
	}
	e.must(svcs[0], f.cmd(t, wagering.KindWin, "win-1", "100.00")) // agora haveria saldo
	again := e.must(svcs[1], c)
	if !again.Replay || again.Transaction.Status() != wagering.StatusRejected || again.Transaction.FailureCode() != wagering.FailInsufficientFunds {
		t.Errorf("replay de rejeição deve devolver a rejeição original: %+v", again.Transaction.Status())
	}
	if e.count(`SELECT count(*) FROM wallet_ledger_entries WHERE transaction_id=$1`, r.Transaction.ID()) != 0 {
		t.Error("rejeição não pode gerar lançamento")
	}

	// carteira de outro jogador e moeda diferente
	wrong := f.cmd(t, wagering.KindBet, "bet-2", "1.00")
	wrong.PlayerID = uuid.NewString()
	if r := e.must(svcs[0], wrong); r.Transaction.FailureCode() != wagering.FailWalletPlayerMismatch {
		t.Errorf("jogador diferente: %s", r.Transaction.FailureCode())
	}
	usd, _ := money.Parse("1.00", "USD")
	cur := f.cmd(t, wagering.KindBet, "bet-3", "1.00")
	cur.Money = usd
	if r := e.must(svcs[0], cur); r.Transaction.FailureCode() != wagering.FailCurrencyMismatch {
		t.Errorf("moeda diferente: %s", r.Transaction.FailureCode())
	}
}

// 50 envios idênticos em paralelo, por três instâncias independentes: um único débito.
func TestSameBet50TimesInParallel(t *testing.T) {
	e := newEnv(t)
	svcs := e.services(3, Config{})
	f := e.wallet(svcs[0], "100.00")
	c := f.cmd(t, wagering.KindBet, "bet-1", "25.00")

	results := make([]Result, 50)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := svcs[i%3].Process(context.Background(), c)
			if err != nil {
				t.Errorf("envio %d: %v", i, err)
				return
			}
			results[i] = r
		}(i)
	}
	wg.Wait()

	originals, replays := 0, 0
	for _, r := range results {
		if r.Transaction == nil {
			continue
		}
		if r.Replay {
			replays++
		} else {
			originals++
		}
		if r.Transaction.ID() != results[0].Transaction.ID() || r.Transaction.ResultBalance().Amount() != "75.00" {
			t.Errorf("resultados divergentes: %s saldo=%s", r.Transaction.ID(), r.Transaction.ResultBalance().Amount())
		}
	}
	if originals != 1 || replays != 49 {
		t.Errorf("esperava 1 original e 49 replays, veio %d e %d", originals, replays)
	}
	if e.ledgerCount(f.walletID, "DEBIT") != 1 || e.balance(f.walletID) != "75.00" {
		t.Errorf("débitos=%d saldo=%s", e.ledgerCount(f.walletID, "DEBIT"), e.balance(f.walletID))
	}
	if e.events(results[0].Transaction.ID(), wagering.EventWalletBalanceChanged) != 1 {
		t.Error("esperava um único WalletBalanceChanged")
	}
	e.assertInvariants(svcs[0], f.walletID)
}

// Teste obrigatório: 100.00 recebe duas apostas distintas de 80.00 ao mesmo tempo.
func TestTwoBetsOf80On100(t *testing.T) {
	e := newEnv(t)
	svcs := e.services(3, Config{})
	for round := 0; round < 25; round++ {
		f := e.wallet(svcs[0], "100.00")
		a, b := f.cmd(t, wagering.KindBet, "bet-a", "80.00"), f.cmd(t, wagering.KindBet, "bet-b", "80.00")

		var wg sync.WaitGroup
		res := make([]Result, 2)
		for i, c := range []wagering.Command{a, b} {
			wg.Add(1)
			go func(i int, c wagering.Command) {
				defer wg.Done()
				r, err := svcs[(round+i)%3].Process(context.Background(), c)
				if err != nil {
					t.Errorf("rodada %d: %v", round, err)
				}
				res[i] = r
			}(i, c)
		}
		wg.Wait()
		if t.Failed() {
			return
		}

		processed, rejected := 0, 0
		for _, r := range res {
			switch r.Transaction.Status() {
			case wagering.StatusProcessed:
				processed++
			case wagering.StatusRejected:
				rejected++
				if r.Transaction.FailureCode() != wagering.FailInsufficientFunds {
					t.Errorf("código da rejeição: %s", r.Transaction.FailureCode())
				}
			}
		}
		if processed != 1 || rejected != 1 {
			t.Fatalf("rodada %d: %d processadas, %d rejeitadas", round, processed, rejected)
		}
		// reenvios não alteram o resultado
		for _, c := range []wagering.Command{a, b} {
			if r := e.must(svcs[(round+1)%3], c); !r.Replay {
				t.Error("reenvio deveria ser replay")
			}
		}
		if e.balance(f.walletID) != "20.00" || e.ledgerCount(f.walletID, "DEBIT") != 1 {
			t.Fatalf("rodada %d: saldo %s, débitos %d", round, e.balance(f.walletID), e.ledgerCount(f.walletID, "DEBIT"))
		}
		e.assertInvariants(svcs[0], f.walletID)
	}
}

// Carteiras independentes avançam em paralelo e nenhuma perde atualização.
func TestIndependentWalletsInParallel(t *testing.T) {
	e := newEnv(t)
	svcs := e.services(3, Config{})
	const wallets, bets = 20, 10

	fx := make([]fixture, wallets)
	for i := range fx {
		fx[i] = e.wallet(svcs[0], "1000.00")
	}
	var wg sync.WaitGroup
	start := time.Now()
	for i := range fx {
		for j := 0; j < bets; j++ {
			wg.Add(1)
			go func(i, j int) {
				defer wg.Done()
				c := fx[i].cmd(t, wagering.KindBet, fmt.Sprintf("bet-%d", j), "10.00")
				if r, err := svcs[(i+j)%3].Process(context.Background(), c); err != nil || r.Transaction.Status() != wagering.StatusProcessed {
					t.Errorf("carteira %d aposta %d: %v", i, j, err)
				}
			}(i, j)
		}
	}
	wg.Wait()
	t.Logf("%d operações em %d carteiras em %s", wallets*bets, wallets, time.Since(start))
	for _, f := range fx {
		if e.balance(f.walletID) != "900.00" || e.ledgerCount(f.walletID, "DEBIT") != bets {
			t.Errorf("carteira %s: saldo %s débitos %d", f.walletID, e.balance(f.walletID), e.ledgerCount(f.walletID, "DEBIT"))
		}
		e.assertInvariants(svcs[0], f.walletID)
	}
}

func TestRefundAndRollbackRules(t *testing.T) {
	e := newEnv(t)
	svc := e.services(1, Config{})[0]
	f := e.wallet(svc, "100.00")

	e.must(svc, f.cmd(t, wagering.KindBet, "bet-1", "25.00")) // 75.00
	refund := e.must(svc, f.ref(t, wagering.KindRefund, "refund-1", "25.00", "bet-1"))
	if refund.Transaction.Status() != wagering.StatusProcessed || e.balance(f.walletID) != "100.00" {
		t.Fatalf("REFUND: %s saldo %s", refund.Transaction.Status(), e.balance(f.walletID))
	}
	if refund.Transaction.ReferenceID() == "" {
		t.Error("a referência interna resolvida deve ser persistida")
	}
	// segunda devolução do mesmo débito, por REFUND ou ROLLBACK
	for _, c := range []wagering.Command{
		f.ref(t, wagering.KindRefund, "refund-2", "25.00", "bet-1"),
		f.ref(t, wagering.KindRollback, "rb-1", "25.00", "bet-1"),
	} {
		r := e.must(svc, c)
		if r.Transaction.Status() != wagering.StatusRejected || r.Transaction.FailureCode() != wagering.FailAlreadyReversed {
			t.Errorf("%s após REFUND: %s %s", c.Kind, r.Transaction.Status(), r.Transaction.FailureCode())
		}
	}
	if e.balance(f.walletID) != "100.00" {
		t.Errorf("saldo mudou por reversões rejeitadas: %s", e.balance(f.walletID))
	}
	// ROLLBACK do REFUND debita de volta
	rbRefund := e.must(svc, f.ref(t, wagering.KindRollback, "rb-2", "25.00", "refund-1"))
	if rbRefund.Transaction.Status() != wagering.StatusProcessed || e.balance(f.walletID) != "75.00" {
		t.Errorf("ROLLBACK de REFUND: %s saldo %s", rbRefund.Transaction.Status(), e.balance(f.walletID))
	}

	// valor diferente
	e.must(svc, f.cmd(t, wagering.KindBet, "bet-2", "10.00")) // 65.00
	if r := e.must(svc, f.ref(t, wagering.KindRefund, "refund-3", "9.99", "bet-2")); r.Transaction.FailureCode() != wagering.FailReversalAmountMismatch {
		t.Errorf("valor parcial: %s", r.Transaction.FailureCode())
	}
	// REFUND só vale para BET
	e.must(svc, f.ref(t, wagering.KindWin, "win-1", "30.00", "bet-2")) // 95.00
	if r := e.must(svc, f.ref(t, wagering.KindRefund, "refund-4", "30.00", "win-1")); r.Transaction.FailureCode() != wagering.FailReferenceKindInvalid {
		t.Errorf("REFUND de WIN: %s", r.Transaction.FailureCode())
	}
	// ROLLBACK de WIN com saldo suficiente debita
	if r := e.must(svc, f.ref(t, wagering.KindRollback, "rb-3", "30.00", "win-1")); r.Transaction.Status() != wagering.StatusProcessed || e.balance(f.walletID) != "65.00" {
		t.Errorf("ROLLBACK de WIN: %s saldo %s", r.Transaction.Status(), e.balance(f.walletID))
	}
	e.assertInvariants(svc, f.walletID)

	// ROLLBACK de WIN sem saldo: código diferente do de BET sem saldo
	g := e.wallet(svc, "0.00")
	e.must(svc, g.cmd(t, wagering.KindWin, "gw-1", "50.00"))
	e.must(svc, g.cmd(t, wagering.KindBet, "gb-1", "50.00"))
	r := e.must(svc, g.ref(t, wagering.KindRollback, "grb-1", "50.00", "gw-1"))
	if r.Transaction.Status() != wagering.StatusRejected || r.Transaction.FailureCode() != wagering.FailReversalInsufficientFunds {
		t.Errorf("reversão sem saldo: %s %s", r.Transaction.Status(), r.Transaction.FailureCode())
	}
	e.assertInvariants(svc, g.walletID)

	// referência de outro provedor não é resolvida por este provedor
	foreign := f.ref(t, wagering.KindRefund, "refund-9", "10.00", "bet-2")
	foreign.ProviderID = "provider-b"
	foreign.IdempotencyKey = "provider-b:refund-9"
	if r := e.must(svc, foreign); r.Transaction.Status() != wagering.StatusPendingReference {
		t.Errorf("referência de outro provedor deve ficar pendente: %s", r.Transaction.Status())
	}
}

// Várias reversões simultâneas da mesma aposta: só uma vence, em qualquer combinação.
func TestConcurrentReversalsOfSameBet(t *testing.T) {
	e := newEnv(t)
	svcs := e.services(3, Config{})
	f := e.wallet(svcs[0], "100.00")
	e.must(svcs[0], f.cmd(t, wagering.KindBet, "bet-1", "25.00"))

	var wg sync.WaitGroup
	var mu sync.Mutex
	processed := 0
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			kind := wagering.KindRefund
			if i%2 == 1 {
				kind = wagering.KindRollback
			}
			r, err := svcs[i%3].Process(context.Background(), f.ref(t, kind, fmt.Sprintf("rev-%d", i), "25.00", "bet-1"))
			if err != nil {
				t.Errorf("reversão %d: %v", i, err)
				return
			}
			if r.Transaction.Status() == wagering.StatusProcessed {
				mu.Lock()
				processed++
				mu.Unlock()
			} else if r.Transaction.FailureCode() != wagering.FailAlreadyReversed {
				t.Errorf("rejeição inesperada: %s", r.Transaction.FailureCode())
			}
		}(i)
	}
	wg.Wait()
	if processed != 1 || e.balance(f.walletID) != "100.00" {
		t.Errorf("reversões processadas=%d saldo=%s", processed, e.balance(f.walletID))
	}
	e.assertInvariants(svcs[0], f.walletID)
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestPendingReferenceResolvedLater(t *testing.T) {
	e := newEnv(t)
	cfg := Config{PendingBaseDelay: time.Second, PendingMaxDelay: time.Second, PendingMaxAttempts: 5}
	clock := &fakeClock{t: time.Now().UTC()}
	base := e.services(3, cfg)
	svcs := make([]*Service, len(base))
	for i, s := range base {
		svcs[i] = s.WithClock(clock.Now)
	}
	ctx := context.Background()
	f := e.wallet(svcs[0], "100.00")

	// a reversão chega ANTES da aposta
	early := e.must(svcs[0], f.ref(t, wagering.KindRefund, "refund-1", "25.00", "bet-1"))
	if early.Transaction.Status() != wagering.StatusPendingReference || early.Transaction.NextAttemptAt() == nil {
		t.Fatalf("esperava PENDING_REFERENCE agendada: %s", early.Transaction.Status())
	}
	if e.events(early.Transaction.ID(), wagering.EventTransactionPendingReference) != 1 {
		t.Error("esperava WagerTransactionPendingReference")
	}
	if e.balance(f.walletID) != "100.00" {
		t.Error("pendência não pode movimentar saldo")
	}
	// reenvio enquanto pendente é replay
	if r := e.must(svcs[1], f.ref(t, wagering.KindRefund, "refund-1", "25.00", "bet-1")); !r.Replay || r.Transaction.Status() != wagering.StatusPendingReference {
		t.Error("replay de pendência deve devolver PENDING_REFERENCE")
	}
	// antes do vencimento nada acontece; depois, ainda sem a aposta, reagenda
	if n, _ := svcs[0].ProcessPendingBatch(ctx, 10); n != 0 {
		t.Errorf("nada deveria vencer ainda, processou %d", n)
	}
	clock.Advance(2 * time.Second)
	svcs[0].ProcessPendingBatch(ctx, 10)
	got, _ := svcs[0].GetTransaction(ctx, early.Transaction.ID())
	if got.Status() != wagering.StatusPendingReference || got.Attempts() != 1 {
		t.Fatalf("após 1ª tentativa sem referência: %s tentativas=%d", got.Status(), got.Attempts())
	}

	// a aposta chega; três workers (três instâncias) disputam a mesma pendência
	e.must(svcs[0], f.cmd(t, wagering.KindBet, "bet-1", "25.00")) // 75.00
	clock.Advance(2 * time.Second)
	var wg sync.WaitGroup
	for _, s := range svcs {
		wg.Add(1)
		go func(s *Service) {
			defer wg.Done()
			if _, err := s.ProcessPendingBatch(ctx, 10); err != nil {
				t.Errorf("worker: %v", err)
			}
		}(s)
	}
	wg.Wait()

	got, _ = svcs[0].GetTransaction(ctx, early.Transaction.ID())
	if got.Status() != wagering.StatusProcessed || got.ResultBalance().Amount() != "100.00" || got.ReferenceID() == "" {
		t.Fatalf("após a aposta chegar: %s", got.Status())
	}
	if e.balance(f.walletID) != "100.00" || e.ledgerCount(f.walletID, "CREDIT") != 2 { // abertura + refund
		t.Errorf("saldo %s créditos %d", e.balance(f.walletID), e.ledgerCount(f.walletID, "CREDIT"))
	}
	if e.events(got.ID(), wagering.EventTransactionProcessed) != 1 || e.events(got.ID(), wagering.EventWalletBalanceChanged) != 1 {
		t.Error("a pendência resolvida deve emitir Processed e BalanceChanged exatamente uma vez")
	}
	e.assertInvariants(svcs[0], f.walletID)
}

func TestPendingReferenceExpires(t *testing.T) {
	e := newEnv(t)
	cfg := Config{PendingBaseDelay: time.Second, PendingMaxDelay: time.Second, PendingMaxAttempts: 3}
	clock := &fakeClock{t: time.Now().UTC()}
	svc := e.services(1, cfg)[0].WithClock(clock.Now)
	ctx := context.Background()
	f := e.wallet(svc, "100.00")

	missing := e.must(svc, f.ref(t, wagering.KindRollback, "rb-1", "25.00", "nunca-chega"))
	// referência existente, mas que terminou sem sucesso
	e.must(svc, f.cmd(t, wagering.KindBet, "bet-big", "500.00")) // rejeitada
	rejectedRef := e.must(svc, f.ref(t, wagering.KindRefund, "refund-2", "500.00", "bet-big"))
	if rejectedRef.Transaction.Status() != wagering.StatusRejected || rejectedRef.Transaction.FailureCode() != wagering.FailReferenceNotProcessed {
		t.Errorf("referência rejeitada deve rejeitar na hora: %s %s", rejectedRef.Transaction.Status(), rejectedRef.Transaction.FailureCode())
	}
	// referência existente, mas ainda pendente: espera e expira com outro código
	waiting := e.must(svc, f.ref(t, wagering.KindRefund, "refund-3", "5.00", "nunca-chega"))
	stuck := e.must(svc, f.ref(t, wagering.KindRefund, "refund-4", "5.00", "refund-3"))
	if stuck.Transaction.Status() != wagering.StatusPendingReference {
		t.Fatalf("referência pendente deve aguardar: %s", stuck.Transaction.Status())
	}

	for i := 0; i < 4; i++ {
		clock.Advance(2 * time.Second)
		if _, err := svc.ProcessPendingBatch(ctx, 10); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		tx   *wagering.Transaction
		code wagering.FailureCode
	}{{missing.Transaction, wagering.FailReferenceNotFound}, {waiting.Transaction, wagering.FailReferenceNotFound}, {stuck.Transaction, wagering.FailReferenceNotProcessed}} {
		got, _ := svc.GetTransaction(ctx, c.tx.ID())
		if got.Status() != wagering.StatusRejected || got.FailureCode() != c.code {
			t.Errorf("%s: esperado REJECTED/%s, veio %s/%s", c.tx.Command().ExternalTransactionID, c.code, got.Status(), got.FailureCode())
		}
		if e.events(got.ID(), wagering.EventTransactionRejected) != 1 {
			t.Errorf("%s: esperava WagerTransactionRejected", got.ID())
		}
	}
	if e.balance(f.walletID) != "100.00" {
		t.Errorf("expirações não movimentam saldo: %s", e.balance(f.walletID))
	}
	// estado terminal: o worker não toca mais
	if n, _ := svc.ProcessPendingBatch(ctx, 10); n != 0 {
		t.Errorf("nada pendente deveria restar, processou %d", n)
	}
	e.assertInvariants(svc, f.walletID)
}

// As garantias vêm do schema, independentemente do código da aplicação.
func TestDatabaseEnforcesInvariants(t *testing.T) {
	e := newEnv(t)
	svc := e.services(1, Config{})[0]
	f := e.wallet(svc, "100.00")
	bet := e.must(svc, f.cmd(t, wagering.KindBet, "bet-1", "30.00"))
	db := e.db()

	mustFail := func(name, wantSubstr, query string, args ...any) {
		t.Helper()
		_, err := db.Exec(query, args...)
		if err == nil {
			t.Errorf("%s: deveria falhar", name)
		} else if !strings.Contains(err.Error(), wantSubstr) {
			t.Errorf("%s: erro inesperado: %v", name, err)
		}
	}

	mustFail("UPDATE do ledger", "append-only", `UPDATE wallet_ledger_entries SET amount=1 WHERE wallet_id=$1`, f.walletID)
	mustFail("DELETE do ledger", "append-only", `DELETE FROM wallet_ledger_entries WHERE wallet_id=$1`, f.walletID)
	mustFail("TRUNCATE do ledger", "append-only", `TRUNCATE wallet_ledger_entries`)
	mustFail("UPDATE de transação terminal", "terminal", `UPDATE wager_transactions SET failure_code=NULL, status='PROCESSED', updated_at=now() WHERE id=$1`, bet.Transaction.ID())
	mustFail("DELETE de transação", "append-only", `DELETE FROM wager_transactions WHERE id=$1`, bet.Transaction.ID())
	mustFail("saldo negativo", "wallets_balance_check", `UPDATE wallets SET balance=-1 WHERE id=$1`, f.walletID)
	mustFail("aritmética do ledger", "ledger_balance_math",
		`INSERT INTO wallet_ledger_entries (id, wallet_id, transaction_id, direction, amount, currency, balance_before, balance_after, created_at)
		 VALUES ($1,$2,$3,'DEBIT',10,'BRL',100,95,now())`, uuid.NewString(), f.walletID, bet.Transaction.ID())
	mustFail("lançamento duplicado por (carteira, transação)", "ux_ledger_wallet_transaction",
		`INSERT INTO wallet_ledger_entries (id, wallet_id, transaction_id, direction, amount, currency, balance_before, balance_after, created_at)
		 VALUES ($1,$2,$3,'DEBIT',10,'BRL',100,90,now())`, uuid.NewString(), f.walletID, bet.Transaction.ID())
	mustFail("LOSS com valor", "wager_amount_by_kind",
		`INSERT INTO wager_transactions (id, origin, kind, status, wallet_id, player_id, amount, currency, provider_id, external_transaction_id,
		   idempotency_key, payload_hash, round_id, game_id, correlation_id, created_at, updated_at, result_balance_after)
		 VALUES ($1,'EXTERNAL','LOSS','PROCESSED',$2,$3,5,'BRL','p','x','k','h','r','g','c',now(),now(),70)`, uuid.NewString(), f.walletID, f.playerID)
	mustFail("segundo crédito inicial", "ux_wager_one_opening_per_wallet",
		`INSERT INTO wager_transactions (id, origin, kind, status, wallet_id, player_id, amount, currency, correlation_id, created_at, updated_at, result_balance_after)
		 VALUES ($1,'INTERNAL','OPENING','PROCESSED',$2,$3,5,'BRL','c',now(),now(),5)`, uuid.NewString(), f.walletID, f.playerID)
	mustFail("OPENING com metadados externos", "wager_origin_shape",
		`INSERT INTO wager_transactions (id, origin, kind, status, wallet_id, player_id, amount, currency, provider_id, correlation_id, created_at, updated_at, result_balance_after)
		 VALUES ($1,'INTERNAL','OPENING','PROCESSED',$2,$3,5,'BRL','p','c',now(),now(),5)`, uuid.NewString(), f.walletID, f.playerID)

	// alterar o saldo sem o lançamento correspondente não passa do commit
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE wallets SET balance=balance+1 WHERE id=$1`, f.walletID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err == nil || !strings.Contains(err.Error(), "does not match ledger") {
		t.Errorf("saldo sem lançamento deveria falhar no commit, veio %v", err)
	}
	if e.balance(f.walletID) != "70.00" {
		t.Errorf("saldo adulterado persistiu: %s", e.balance(f.walletID))
	}
	e.assertInvariants(svc, f.walletID)
}

func TestLedgerPaginationAndReconciliation(t *testing.T) {
	e := newEnv(t)
	svc := e.services(1, Config{})[0]
	ctx := context.Background()
	f := e.wallet(svc, "500.00")
	for i := 0; i < 7; i++ {
		e.must(svc, f.cmd(t, wagering.KindBet, fmt.Sprintf("bet-%d", i), "10.00"))
	}
	var seen []string
	var after int64
	for {
		rows, err := svc.LedgerPage(ctx, f.walletID, after, 3)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 0 {
			break
		}
		for _, r := range rows {
			seen = append(seen, r.Entry.ID())
			after = r.Seq
		}
	}
	if len(seen) != 8 { // abertura + 7 apostas
		t.Errorf("paginação devolveu %d lançamentos, esperado 8", len(seen))
	}
	uniq := map[string]bool{}
	for _, id := range seen {
		uniq[id] = true
	}
	if len(uniq) != len(seen) {
		t.Error("paginação repetiu lançamentos")
	}
	r, err := svc.Reconcile(ctx, f.walletID)
	if err != nil || !r.Consistent || r.CheckedEntries != 8 || r.Stored.Amount() != "430.00" || r.Difference.Amount() != "0.00" {
		t.Errorf("reconciliação: %+v %v", r, err)
	}
	if _, err := svc.Reconcile(ctx, uuid.NewString()); err != ErrWalletNotFound {
		t.Errorf("reconciliação de carteira inexistente: %v", err)
	}
}
