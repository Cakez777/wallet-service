//go:build integration

package bootstrap

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Cakez777/wallet-service/internal/app"
	"github.com/Cakez777/wallet-service/internal/testutil"
	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func setEnv(t *testing.T, dsn, addr string) {
	t.Helper()
	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("HTTP_ADDR", addr)
	t.Setenv("OIDC_ISSUER", "http://idp.test/realms/wallet")
	t.Setenv("OIDC_JWKS_URL", "http://127.0.0.1:1/jwks") // IdP inalcançável: só /health é público
	t.Setenv("WORKER_INTERVAL_MS", "50")
	t.Setenv("MESSAGING_ENABLED", "false")
}

func get(t *testing.T, url string) int {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer res.Body.Close()
	return res.StatusCode
}

func TestLifecycleStartAndGracefulStop(t *testing.T) {
	dsn := testutil.NewDatabase(t)
	addr := freeAddr(t)
	setEnv(t, dsn, addr)

	var db *sql.DB
	var svc *app.Service
	a := fxtest.New(t, Module(), fx.Populate(&db, &svc))
	a.RequireStart()

	if c := get(t, "http://"+addr+"/health/live"); c != 200 {
		t.Errorf("live: %d", c)
	}
	if c := get(t, "http://"+addr+"/health/ready"); c != 200 {
		t.Errorf("ready: %d", c)
	}
	if c := get(t, "http://"+addr+"/wallets/x"); c != 401 {
		t.Errorf("endpoint de negócio sem token deve dar 401, veio %d", c)
	}
	// o worker está rodando e usando o banco
	time.Sleep(150 * time.Millisecond)
	if _, err := svc.ProcessPendingBatch(context.Background(), 1); err != nil {
		t.Errorf("worker/banco deveriam estar ativos: %v", err)
	}

	a.RequireStop()

	// depois do shutdown: sem novas entradas, e recursos liberados
	if _, err := http.Get("http://" + addr + "/health/live"); err == nil {
		t.Error("o servidor ainda aceita conexões após o encerramento")
	}
	if err := db.Ping(); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Errorf("o pool do banco deveria estar fechado, Ping: %v", err)
	}
	if _, err := svc.ProcessPendingBatch(context.Background(), 1); err == nil {
		t.Error("o serviço não deveria mais alcançar o banco")
	}
}

func TestStartupValidatesConfigAndDependencies(t *testing.T) {
	// 1. configuração do IdP ausente: nem a composição é aceita
	t.Setenv("OIDC_ISSUER", "")
	t.Setenv("OIDC_JWKS_URL", "")
	if err := fx.ValidateApp(Module()); err != nil {
		t.Fatalf("o grafo de dependências deve ser válido: %v", err)
	}
	app1 := fx.New(Module(), fx.NopLogger)
	if err := app1.Err(); err == nil || !strings.Contains(err.Error(), "OIDC_ISSUER") {
		t.Errorf("sem OIDC_* a aplicação não pode subir, erro: %v", err)
	}

	// 2. valor numérico inválido
	setEnv(t, "postgres://wallet@127.0.0.1:1/x?sslmode=disable", "127.0.0.1:0")
	t.Setenv("PENDING_MAX_ATTEMPTS", "abc")
	if err := fx.New(Module(), fx.NopLogger).Err(); err == nil || !strings.Contains(err.Error(), "PENDING_MAX_ATTEMPTS") {
		t.Errorf("valor inválido deve ser recusado: %v", err)
	}

	// 3. PostgreSQL inalcançável: a inicialização falha em vez de subir "pela metade"
	t.Setenv("PENDING_MAX_ATTEMPTS", "3")
	a := fx.New(Module(), fx.NopLogger)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.Start(ctx); err == nil || !strings.Contains(err.Error(), "postgres unreachable") {
		t.Errorf("sem PostgreSQL a inicialização deveria falhar, erro: %v", err)
	}
	_ = a.Stop(context.Background())

	// 4. porta ocupada: erro de bind falha a inicialização
	dsn := testutil.NewDatabase(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	setEnv(t, dsn, ln.Addr().String())
	b := fx.New(Module(), fx.NopLogger)
	if err := b.Start(ctx); err == nil || !strings.Contains(fmt.Sprint(err), "bind") {
		t.Errorf("porta ocupada deveria falhar a inicialização, erro: %v", err)
	}
	_ = b.Stop(context.Background())
}
