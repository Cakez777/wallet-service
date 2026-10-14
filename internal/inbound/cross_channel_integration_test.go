//go:build integration

package inbound

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Cakez777/wallet-service/internal/app"
	"github.com/Cakez777/wallet-service/internal/auth"
	"github.com/Cakez777/wallet-service/internal/httpapi"
	"github.com/Cakez777/wallet-service/internal/wagering"
)

type staticIdP struct{}

func (staticIdP) Verify(_ context.Context, tok string) (auth.Principal, error) {
	if tok == "tok-a" {
		return auth.Principal{Subject: "sa-a", ClientID: "provider-a", Roles: []string{auth.RoleProvider}}, nil
	}
	return auth.Principal{}, auth.ErrUnauthenticated
}

func (f *fixture) httpServer(svc *app.Service) *httptest.Server {
	api := httpapi.New(svc, staticIdP{}, func(ctx context.Context) error { return f.db.PingContext(ctx) })
	srv := httptest.NewServer(api.Handler())
	f.t.Cleanup(srv.Close)
	return srv
}

// postHTTP envia a mesma operação do CommandMessage pelo HTTP e devolve status, corpo e se foi replay.
func postHTTP(t *testing.T, srv *httptest.Server, m CommandMessage) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"providerId": m.ProviderID, "externalTransactionId": m.ExternalTransactionID, "playerId": m.PlayerID,
		"walletId": m.WalletID, "roundId": m.RoundID, "gameId": m.GameID, "kind": m.Kind,
		"money": map[string]string{"amount": m.Money.Amount, "currency": m.Money.Currency},
	})
	req, _ := http.NewRequest("POST", srv.URL+"/wagering/transactions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok-a")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", m.IdempotencyKey)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func (f *fixture) debits() int {
	return f.count(`SELECT COUNT(*) FROM wallet_ledger_entries WHERE wallet_id=$1 AND direction='DEBIT'`, f.walID)
}

// A mesma operação de negócio chega por HTTP e por SQS (mensagens com messageId distintos):
// só um dos canais aplica o efeito; o outro recebe o resultado original.
func TestSameOperationViaHTTPAndSQSIsAppliedOnce(t *testing.T) {
	for _, httpFirst := range []bool{true, false} {
		name := "sqs-depois-http"
		if httpFirst {
			name = "http-depois-sqs"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, "100.00", 5)
			srv := f.httpServer(f.svc)
			m := f.msg("BET", "30.00", 1)
			m.IdempotencyKey = "provider-a:cross-1"
			m.MessageID = "msg-cross-" + name
			f.start(f.svc, 2)

			viaHTTP := func() {
				status, out := postHTTP(t, srv, m)
				if status != 200 {
					t.Fatalf("HTTP status = %d (%v), esperado 200", status, out)
				}
			}
			viaSQS := func() {
				f.publish(m, "")
				eventually(t, 10*time.Second, "fila esvaziar", func() bool { return f.q.Depth() == 0 })
			}

			if httpFirst {
				viaHTTP()
				viaSQS()
			} else {
				viaSQS()
				status, out := postHTTP(t, srv, m)
				if status != 200 || out["idempotentReplay"] != true {
					t.Fatalf("HTTP depois do SQS: status=%d replay=%v, esperado 200 e replay", status, out["idempotentReplay"])
				}
			}

			if got := f.balance(); got != "70.00" {
				t.Errorf("saldo = %s, esperado 70.00 (um único débito)", got)
			}
			if n := f.debits(); n != 1 {
				t.Errorf("débitos no ledger = %d, esperado 1", n)
			}
			if n := f.count(`SELECT COUNT(*) FROM wager_transactions WHERE wallet_id=$1 AND kind='BET'`, f.walID); n != 1 {
				t.Errorf("transações BET = %d, esperado 1", n)
			}
		})
	}
}

// "Reinício": novo pool de conexões e novo Service (nada em memória sobrevive). A idempotência e as
// pendências vêm só do PostgreSQL.
func TestRestartKeepsIdempotencyAndPendingReferences(t *testing.T) {
	f := newFixture(t, "100.00", 5)
	cfg := app.Config{PendingBaseDelay: 10 * time.Millisecond, PendingMaxDelay: 20 * time.Millisecond, PendingMaxAttempts: 50}
	ctx := context.Background()

	// reopen simula o reinício do processo: descarta o pool e o Service e abre tudo de novo.
	var current *sql.DB
	reopen := func() *app.Service {
		if current != nil {
			current.Close()
		}
		db, err := sql.Open("pgx", f.dsn)
		if err != nil {
			t.Fatal(err)
		}
		current = db
		f.db = db // as consultas do teste passam a usar o pool novo
		return app.NewService(db, cfg)
	}
	t.Cleanup(func() {
		if current != nil {
			current.Close()
		}
	})

	cmdOf := func(m CommandMessage) wagering.Command {
		c, _, err := decodeEnvelope(Encode(m))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	// 1) Antes do "reinício": uma BET e uma REFUND cuja referência ainda não existe.
	svc1 := reopen()
	bet := f.msg("BET", "20.00", 1)
	r1, err := svc1.Process(ctx, cmdOf(bet))
	if err != nil || r1.Transaction.Status() != wagering.StatusProcessed {
		t.Fatalf("BET: %v status=%v", err, r1.Transaction)
	}
	refund := f.msg("REFUND", "15.00", 2)
	refund.ReferenceExternalTransactionID = "ext-futura"
	rp, err := svc1.Process(ctx, cmdOf(refund))
	if err != nil || rp.Transaction.Status() != wagering.StatusPendingReference {
		t.Fatalf("REFUND deveria ficar pendente: %v", err)
	}

	// 2) "Reinício": todos os objetos em memória são descartados.
	svc1 = nil
	svc2 := reopen()

	// idempotência: repetir a BET devolve o resultado original, sem novo débito
	r2, err := svc2.Process(ctx, cmdOf(bet))
	if err != nil || !r2.Replay || r2.Transaction.ID() != r1.Transaction.ID() {
		t.Fatalf("replay após reinício: err=%v replay=%v", err, r2.Replay)
	}
	if got := f.balance(); got != "80.00" {
		t.Fatalf("saldo = %s, esperado 80.00", got)
	}

	// a referência chega depois do reinício; o novo Service resolve a pendência sozinho
	future := f.msg("BET", "15.00", 3)
	future.ExternalTransactionID = "ext-futura"
	future.IdempotencyKey = "key-ext-futura"
	if _, err := svc2.Process(ctx, cmdOf(future)); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "pendência resolvida", func() bool {
		if _, err := svc2.ProcessPendingBatch(ctx, 10); err != nil {
			t.Fatal(err)
		}
		return f.count(`SELECT COUNT(*) FROM wager_transactions WHERE id=$1 AND status='PROCESSED'`, rp.Transaction.ID()) == 1
	})
	// 100 − 20 (BET) − 15 (BET futura) + 15 (REFUND) = 80
	if got := f.balance(); got != "80.00" {
		t.Errorf("saldo final = %s, esperado 80.00", got)
	}
}
