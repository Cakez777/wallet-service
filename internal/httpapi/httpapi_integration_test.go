//go:build integration

package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Cakez777/wallet-service/internal/app"
	"github.com/Cakez777/wallet-service/internal/auth"
	"github.com/Cakez777/wallet-service/internal/testutil"
	"github.com/google/uuid"
)

// fakeIdP mapeia tokens de teste para identidades. A validação criptográfica real
// (JWKS, assinatura, expiração, emissor) é testada em internal/auth e, contra o Keycloak
// de verdade, em internal/auth/keycloak_test.go.
type fakeIdP struct {
	down atomic.Bool
	ids  map[string]auth.Principal
}

func (f *fakeIdP) Verify(_ context.Context, token string) (auth.Principal, error) {
	if f.down.Load() {
		return auth.Principal{}, auth.ErrIdPUnavailable
	}
	if p, ok := f.ids[token]; ok {
		return p, nil
	}
	return auth.Principal{}, auth.ErrUnauthenticated
}

type env struct {
	t   *testing.T
	srv *httptest.Server
	db  *sql.DB
	idp *fakeIdP
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dsn := testutil.NewDatabase(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	idp := &fakeIdP{ids: map[string]auth.Principal{
		"tok-a":        {Subject: "sa-a", ClientID: "provider-a", Roles: []string{auth.RoleProvider}},
		"tok-b":        {Subject: "sa-b", ClientID: "provider-b", Roles: []string{auth.RoleProvider}},
		"tok-internal": {Subject: "sa-i", ClientID: "wallet-internal-service", Roles: []string{auth.RoleInternal}},
		"tok-norole":   {Subject: "sa-n", ClientID: "provider-a", Roles: nil},
	}}
	api := New(app.NewService(db, app.Config{}), idp, func(ctx context.Context) error { return db.PingContext(ctx) })
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return &env{t: t, srv: srv, db: db, idp: idp}
}

type resp struct {
	status int
	header http.Header
	body   map[string]any
}

func (e *env) do(method, path, token string, headers map[string]string, body any) resp {
	e.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	out := resp{status: res.StatusCode, header: res.Header}
	_ = json.NewDecoder(res.Body).Decode(&out.body)
	return out
}

func (e *env) wantStatus(r resp, want int, what string) resp {
	e.t.Helper()
	if r.status != want {
		e.t.Fatalf("%s: esperado HTTP %d, veio %d (%v)", what, want, r.status, r.body)
	}
	return r
}

func errCode(r resp) string {
	if m, ok := r.body["error"].(map[string]any); ok {
		s, _ := m["code"].(string)
		return s
	}
	return ""
}

func brl(amount string) map[string]string {
	return map[string]string{"amount": amount, "currency": "BRL"}
}

func (e *env) newWallet(balance string) (walletID, playerID string) {
	e.t.Helper()
	playerID = uuid.NewString()
	r := e.do("POST", "/wallets", "tok-internal", nil, map[string]any{"playerId": playerID, "initialBalance": brl(balance)})
	e.wantStatus(r, 201, "POST /wallets")
	return r.body["id"].(string), playerID
}

func submit(walletID, playerID, ext, kind, amount string) map[string]any {
	return map[string]any{
		"providerId": "provider-a", "externalTransactionId": ext, "playerId": playerID, "walletId": walletID,
		"roundId": "round-1", "gameId": "fortune-chimp", "kind": kind, "money": brl(amount),
	}
}

func key(ext string) map[string]string {
	return map[string]string{"Idempotency-Key": "provider-a:" + ext}
}

func (e *env) count(q string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(q, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestAuthenticationAndAuthorization(t *testing.T) {
	e := newEnv(t)
	walletID, playerID := e.newWallet("100.00")
	before := e.count(`SELECT count(*) FROM wallet_ledger_entries`)

	// health é público
	e.wantStatus(e.do("GET", "/health/live", "", nil, nil), 200, "live")
	e.wantStatus(e.do("GET", "/health/ready", "", nil, nil), 200, "ready")

	endpoints := []struct{ method, path string }{
		{"POST", "/wallets"}, {"GET", "/wallets/" + walletID}, {"GET", "/wallets/" + walletID + "/ledger"},
		{"POST", "/wallets/" + walletID + "/reconciliation"}, {"POST", "/wagering/transactions"},
		{"GET", "/wagering/transactions/" + uuid.NewString()},
		{"GET", "/providers/provider-a/wagering/transactions/x"},
	}
	for _, ep := range endpoints {
		for name, tok := range map[string]string{"sem token": "", "token inválido": "lixo"} {
			r := e.do(ep.method, ep.path, tok, nil, nil)
			if r.status != 401 || errCode(r) != "UNAUTHORIZED" || r.header.Get("WWW-Authenticate") == "" {
				t.Errorf("%s %s (%s): esperado 401, veio %d %v", ep.method, ep.path, name, r.status, r.body)
			}
		}
		if r := e.do(ep.method, ep.path, "tok-norole", nil, nil); r.status != 403 {
			t.Errorf("%s %s sem papel: esperado 403, veio %d", ep.method, ep.path, r.status)
		}
	}
	// o esquema precisa ser Bearer
	if r := e.do("GET", "/wallets/"+walletID, "", map[string]string{"Authorization": "Basic dG9rLWludGVybmFs"}, nil); r.status != 401 {
		t.Errorf("Basic não deve autenticar: %d", r.status)
	}

	// operações de carteira: só o serviço interno
	for _, tok := range []string{"tok-a", "tok-b"} {
		e.wantStatus(e.do("POST", "/wallets", tok, nil, map[string]any{"playerId": uuid.NewString(), "initialBalance": brl("5.00")}), 403, "provedor abrindo carteira")
		e.wantStatus(e.do("GET", "/wallets/"+walletID, tok, nil, nil), 403, "provedor lendo carteira")
		e.wantStatus(e.do("GET", "/wallets/"+walletID+"/ledger", tok, nil, nil), 403, "provedor lendo ledger")
		e.wantStatus(e.do("POST", "/wallets/"+walletID+"/reconciliation", tok, nil, nil), 403, "provedor reconciliando")
	}
	// o serviço interno não envia apostas
	e.wantStatus(e.do("POST", "/wagering/transactions", "tok-internal", key("x1"), submit(walletID, playerID, "x1", "BET", "1.00")), 403, "interno enviando aposta")

	// nenhum acesso não autorizado teve efeito financeiro
	if after := e.count(`SELECT count(*) FROM wallet_ledger_entries`); after != before {
		t.Errorf("acessos não autorizados alteraram o ledger: %d -> %d", before, after)
	}
	if n := e.count(`SELECT count(*) FROM wager_transactions WHERE wallet_id=$1 AND kind<>'OPENING'`, walletID); n != 0 {
		t.Errorf("acessos não autorizados criaram %d transações", n)
	}

	// IdP indisponível: 503, não 401
	e.idp.down.Store(true)
	r := e.do("GET", "/wallets/"+walletID, "tok-internal", nil, nil)
	if r.status != 503 || r.header.Get("Retry-After") == "" || errCode(r) != "IDENTITY_PROVIDER_UNAVAILABLE" {
		t.Errorf("IdP fora: esperado 503, veio %d %v", r.status, r.body)
	}
}

func TestSubmitContractAndStatusCodes(t *testing.T) {
	e := newEnv(t)
	walletID, playerID := e.newWallet("100.00")

	// processada
	r := e.wantStatus(e.do("POST", "/wagering/transactions", "tok-a", key("bet-1"), submit(walletID, playerID, "bet-1", "BET", "25.00")), 200, "BET")
	if r.body["status"] != "PROCESSED" || r.body["idempotentReplay"] != false || r.body["transactionId"] == "" {
		t.Fatalf("BET: %v", r.body)
	}
	if b := r.body["balance"].(map[string]any); b["amount"] != "75.00" || b["currency"] != "BRL" {
		t.Errorf("saldo: %v", b)
	}
	txID := r.body["transactionId"].(string)

	// replay devolve o resultado original
	r2 := e.wantStatus(e.do("POST", "/wagering/transactions", "tok-a", key("bet-1"), submit(walletID, playerID, "bet-1", "BET", "25.00")), 200, "replay")
	if r2.body["idempotentReplay"] != true || r2.body["transactionId"] != txID {
		t.Errorf("replay: %v", r2.body)
	}

	// 400: entrada inválida, nada persistido
	cases := map[string]struct {
		headers map[string]string
		body    any
		code    string
	}{
		"sem Idempotency-Key": {nil, submit(walletID, playerID, "b2", "BET", "1.00"), "MISSING_IDEMPOTENCY_KEY"},
		"JSON inválido":       {key("b3"), "{nao-json", "INVALID_REQUEST"},
		"campo desconhecido":  {key("b4"), map[string]any{"providerId": "provider-a", "player_id": "x"}, "INVALID_REQUEST"},
		"valor sem escala":    {key("b5"), submit(walletID, playerID, "b5", "BET", "25"), "INVALID_REQUEST"},
		"valor negativo":      {key("b6"), submit(walletID, playerID, "b6", "BET", "-1.00"), "INVALID_REQUEST"},
		"notação científica":  {key("b7"), submit(walletID, playerID, "b7", "BET", "1e2"), "INVALID_REQUEST"},
		"OPENING externo":     {key("b8"), submit(walletID, playerID, "b8", "OPENING", "1.00"), "INVALID_REQUEST"},
		"LOSS com valor":      {key("b9"), submit(walletID, playerID, "b9", "LOSS", "1.00"), "INVALID_REQUEST"},
		"REFUND sem ref":      {key("b10"), submit(walletID, playerID, "b10", "REFUND", "1.00"), "INVALID_REQUEST"},
	}
	for name, c := range cases {
		got := e.do("POST", "/wagering/transactions", "tok-a", c.headers, c.body)
		if got.status != 400 || errCode(got) != c.code {
			t.Errorf("%s: esperado 400/%s, veio %d %v", name, c.code, got.status, got.body)
		}
	}
	if n := e.count(`SELECT count(*) FROM wager_transactions WHERE wallet_id=$1 AND kind<>'OPENING'`, walletID); n != 1 {
		t.Errorf("entradas inválidas não devem persistir nada, há %d transações", n)
	}

	// 403: o providerId precisa ser o autenticado
	spoof := submit(walletID, playerID, "b11", "BET", "1.00")
	spoof["providerId"] = "provider-b"
	if got := e.do("POST", "/wagering/transactions", "tok-a", key("b11"), spoof); got.status != 403 || errCode(got) != "PROVIDER_MISMATCH" {
		t.Errorf("providerId forjado: %d %v", got.status, got.body)
	}

	// 409: mesma chave com conteúdo diferente; mesma operação com outra chave
	if got := e.do("POST", "/wagering/transactions", "tok-a", key("bet-1"), submit(walletID, playerID, "bet-1", "BET", "26.00")); got.status != 409 || errCode(got) != "IDEMPOTENCY_KEY_CONFLICT" {
		t.Errorf("conflito de chave: %d %v", got.status, got.body)
	}
	if got := e.do("POST", "/wagering/transactions", "tok-a", map[string]string{"Idempotency-Key": "outra"}, submit(walletID, playerID, "bet-1", "BET", "25.00")); got.status != 409 || errCode(got) != "EXTERNAL_TRANSACTION_CONFLICT" {
		t.Errorf("conflito de operação: %d %v", got.status, got.body)
	}

	// 422: rejeição de negócio, com código estável; replay mantém a rejeição
	rej := e.wantStatus(e.do("POST", "/wagering/transactions", "tok-a", key("bet-big"), submit(walletID, playerID, "bet-big", "BET", "500.00")), 422, "sem saldo")
	if rej.body["status"] != "REJECTED" || rej.body["failureCode"] != "INSUFFICIENT_FUNDS" {
		t.Errorf("rejeição: %v", rej.body)
	}
	rej2 := e.wantStatus(e.do("POST", "/wagering/transactions", "tok-a", key("bet-big"), submit(walletID, playerID, "bet-big", "BET", "500.00")), 422, "replay da rejeição")
	if rej2.body["idempotentReplay"] != true {
		t.Errorf("replay da rejeição: %v", rej2.body)
	}

	// 202: referência ainda não chegou
	ref := submit(walletID, playerID, "refund-1", "REFUND", "10.00")
	ref["referenceExternalTransactionId"] = "bet-futura"
	pend := e.wantStatus(e.do("POST", "/wagering/transactions", "tok-a", key("refund-1"), ref), 202, "referência pendente")
	if pend.body["status"] != "PENDING_REFERENCE" || pend.body["nextAttemptAt"] == nil {
		t.Errorf("pendência: %v", pend.body)
	}

	// 404: carteira inexistente
	if got := e.do("POST", "/wagering/transactions", "tok-a", key("b12"), submit(uuid.NewString(), playerID, "b12", "BET", "1.00")); got.status != 404 || errCode(got) != "WALLET_NOT_FOUND" {
		t.Errorf("carteira inexistente: %d %v", got.status, got.body)
	}

	// correlationId ecoado
	got := e.do("POST", "/wagering/transactions", "tok-a", map[string]string{"Idempotency-Key": "provider-a:w1", "X-Correlation-Id": "corr-xyz"}, submit(walletID, playerID, "w1", "WIN", "5.00"))
	if got.header.Get("X-Correlation-Id") != "corr-xyz" {
		t.Errorf("X-Correlation-Id não ecoado: %q", got.header.Get("X-Correlation-Id"))
	}
	if n := e.count(`SELECT count(*) FROM wager_transactions WHERE correlation_id='corr-xyz'`); n != 1 {
		t.Errorf("correlationId não persistido: %d", n)
	}
}

func TestProviderIsolation(t *testing.T) {
	e := newEnv(t)
	walletID, playerID := e.newWallet("100.00")
	r := e.wantStatus(e.do("POST", "/wagering/transactions", "tok-a", key("bet-1"), submit(walletID, playerID, "bet-1", "BET", "10.00")), 200, "BET do provedor A")
	txID := r.body["transactionId"].(string)

	// o dono consulta pelos dois caminhos
	own := e.wantStatus(e.do("GET", "/wagering/transactions/"+txID, "tok-a", nil, nil), 200, "dono por id")
	if own.body["providerId"] != "provider-a" || own.body["kind"] != "BET" || own.body["status"] != "PROCESSED" {
		t.Errorf("detalhe: %v", own.body)
	}
	e.wantStatus(e.do("GET", "/providers/provider-a/wagering/transactions/bet-1", "tok-a", nil, nil), 200, "dono por id externo")

	// outro provedor não vê nada, inclusive replays
	if got := e.do("GET", "/wagering/transactions/"+txID, "tok-b", nil, nil); got.status != 404 {
		t.Errorf("provedor B por id: esperado 404 (sem vazar existência), veio %d", got.status)
	}
	if got := e.do("GET", "/providers/provider-a/wagering/transactions/bet-1", "tok-b", nil, nil); got.status != 403 {
		t.Errorf("provedor B no caminho do A: esperado 403, veio %d", got.status)
	}
	if got := e.do("GET", "/providers/provider-b/wagering/transactions/bet-1", "tok-b", nil, nil); got.status != 404 {
		t.Errorf("provedor B no próprio caminho: esperado 404, veio %d", got.status)
	}
	replayAsB := submit(walletID, playerID, "bet-1", "BET", "10.00") // providerId = provider-a
	if got := e.do("POST", "/wagering/transactions", "tok-b", key("bet-1"), replayAsB); got.status != 403 {
		t.Errorf("replay por outro provedor: esperado 403, veio %d", got.status)
	}
	// o mesmo id externo em outro provedor é outra operação
	asB := submit(walletID, playerID, "bet-1", "BET", "10.00")
	asB["providerId"] = "provider-b"
	got := e.wantStatus(e.do("POST", "/wagering/transactions", "tok-b", map[string]string{"Idempotency-Key": "provider-b:bet-1"}, asB), 200, "mesmo id externo, outro provedor")
	if got.body["idempotentReplay"] != false || got.body["transactionId"] == txID {
		t.Errorf("deveria ser uma transação nova: %v", got.body)
	}
	// o serviço interno enxerga tudo
	e.wantStatus(e.do("GET", "/wagering/transactions/"+txID, "tok-internal", nil, nil), 200, "interno por id")
	e.wantStatus(e.do("GET", "/providers/provider-a/wagering/transactions/bet-1", "tok-internal", nil, nil), 200, "interno por caminho")
}

func TestWalletEndpoints(t *testing.T) {
	e := newEnv(t)
	player := uuid.NewString()
	r := e.wantStatus(e.do("POST", "/wallets", "tok-internal", nil, map[string]any{"playerId": player, "initialBalance": brl("1000.00")}), 201, "abrir")
	if r.body["playerId"] != player || r.body["version"] != float64(1) || r.body["balance"].(map[string]any)["amount"] != "1000.00" {
		t.Fatalf("resposta: %v", r.body)
	}
	walletID := r.body["id"].(string)
	// mesmo jogador e moeda: conflito
	if got := e.do("POST", "/wallets", "tok-internal", nil, map[string]any{"playerId": player, "initialBalance": brl("1.00")}); got.status != 409 {
		t.Errorf("carteira repetida: esperado 409, veio %d", got.status)
	}
	// snake_case antigo e saldo inválido
	if got := e.do("POST", "/wallets", "tok-internal", nil, map[string]any{"player_id": player}); got.status != 400 {
		t.Errorf("snake_case: esperado 400, veio %d", got.status)
	}
	if got := e.do("POST", "/wallets", "tok-internal", nil, map[string]any{"playerId": uuid.NewString(), "initialBalance": brl("10.5")}); got.status != 400 {
		t.Errorf("escala inválida: esperado 400, veio %d", got.status)
	}
	if got := e.do("GET", "/wallets/"+uuid.NewString(), "tok-internal", nil, nil); got.status != 404 {
		t.Errorf("carteira inexistente: %d", got.status)
	}

	for i := 0; i < 5; i++ {
		ext := "bet-" + string(rune('a'+i))
		e.wantStatus(e.do("POST", "/wagering/transactions", "tok-a", key(ext), submit(walletID, player, ext, "BET", "10.00")), 200, "BET")
	}
	// ledger paginado por cursor opaco: abertura + 5 apostas = 6 lançamentos
	var seen []string
	path := "/wallets/" + walletID + "/ledger?limit=4"
	for pages := 0; pages < 5; pages++ {
		page := e.wantStatus(e.do("GET", path, "tok-internal", nil, nil), 200, "ledger")
		for _, it := range page.body["items"].([]any) {
			m := it.(map[string]any)
			seen = append(seen, m["id"].(string))
			for _, k := range []string{"transactionId", "direction", "money", "balanceBefore", "balanceAfter"} {
				if m[k] == nil {
					t.Errorf("lançamento sem %s", k)
				}
			}
		}
		next, _ := page.body["nextCursor"].(string)
		if next == "" {
			break
		}
		path = "/wallets/" + walletID + "/ledger?limit=4&cursor=" + next
	}
	if len(seen) != 6 {
		t.Errorf("paginação devolveu %d lançamentos, esperado 6", len(seen))
	}
	for _, bad := range []string{"cursor=%%%", "cursor=" + "YWJj", "limit=0", "limit=201", "limit=x"} {
		if got := e.do("GET", "/wallets/"+walletID+"/ledger?"+bad, "tok-internal", nil, nil); got.status != 400 {
			t.Errorf("%s: esperado 400, veio %d", bad, got.status)
		}
	}

	rec := e.wantStatus(e.do("POST", "/wallets/"+walletID+"/reconciliation", "tok-internal", nil, nil), 200, "reconciliação")
	if rec.body["consistent"] != true || rec.body["checkedEntries"] != float64(6) ||
		rec.body["storedBalance"].(map[string]any)["amount"] != "950.00" || rec.body["difference"].(map[string]any)["amount"] != "0.00" {
		t.Errorf("reconciliação: %v", rec.body)
	}
	w := e.wantStatus(e.do("GET", "/wallets/"+walletID, "tok-internal", nil, nil), 200, "consulta")
	if w.body["version"] != float64(6) || w.body["balance"].(map[string]any)["amount"] != "950.00" {
		t.Errorf("carteira: %v", w.body)
	}
}
