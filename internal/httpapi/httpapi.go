// Package httpapi expõe os casos de uso por HTTP, com autenticação e autorização.
//
// Autorização:
//   - provedor (papel wallet-provider): envia operações e consulta apenas as suas; o providerId
//     do corpo/URL precisa ser o do token. Transações de outros provedores respondem 404.
//   - serviço interno (papel wallet-internal): operações de carteira (abertura, consulta,
//     ledger, reconciliação) e leitura de qualquer transação.
//   - /health/* é público.
package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Cakez777/wallet-service/internal/app"
	"github.com/Cakez777/wallet-service/internal/auth"
	"github.com/Cakez777/wallet-service/internal/money"
	"github.com/Cakez777/wallet-service/internal/wagering"
	"github.com/Cakez777/wallet-service/internal/wallet"
	"github.com/google/uuid"
)

// TokenVerifier valida um token Bearer.
type TokenVerifier interface {
	Verify(ctx context.Context, token string) (auth.Principal, error)
}

// API reúne as dependências dos handlers.
type API struct {
	svc      *app.Service
	verifier TokenVerifier
	ready    func(ctx context.Context) error
	metrics  http.Handler
}

// WithMetrics publica GET /metrics (formato Prometheus), sem autenticação, como /health/*.
// Em produção, restrinja o acesso por rede (a porta não deve ser exposta publicamente).
func (a *API) WithMetrics(h http.Handler) *API { a.metrics = h; return a }

// New cria a API. ready verifica as dependências (PostgreSQL) para /health/ready.
func New(svc *app.Service, v TokenVerifier, ready func(ctx context.Context) error) *API {
	return &API{svc: svc, verifier: v, ready: ready}
}

type ctxKey int

const (
	principalKey ctxKey = iota
	correlationKey
)

// Handler devolve o roteador completo.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /health/ready", a.handleReady)
	if a.metrics != nil {
		mux.Handle("GET /metrics", a.metrics)
	}

	internal := func(h http.HandlerFunc) http.Handler { return a.require(auth.RoleInternal, h) }
	provider := func(h http.HandlerFunc) http.Handler { return a.require(auth.RoleProvider, h) }
	either := func(h http.HandlerFunc) http.Handler { return a.require("", h) }

	mux.Handle("POST /wallets", internal(a.handleCreateWallet))
	mux.Handle("GET /wallets/{walletId}", internal(a.handleGetWallet))
	mux.Handle("GET /wallets/{walletId}/ledger", internal(a.handleLedger))
	mux.Handle("POST /wallets/{walletId}/reconciliation", internal(a.handleReconcile))

	mux.Handle("POST /wagering/transactions", provider(a.handleSubmit))
	mux.Handle("GET /wagering/transactions/{transactionId}", either(a.handleGetTransaction))
	mux.Handle("GET /providers/{providerId}/wagering/transactions/{externalTransactionId}", either(a.handleGetProviderTransaction))
	return withCorrelation(mux)
}

// ---------------------------------------------------------------------------
// Infraestrutura HTTP
// ---------------------------------------------------------------------------

type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	var b errorBody
	b.Error.Code, b.Error.Message = code, msg
	writeJSON(w, status, b)
}

func withCorrelation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Correlation-Id")
		if id == "" || len(id) > 128 {
			id = uuid.NewString()
		}
		w.Header().Set("X-Correlation-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), correlationKey, id)))
	})
}

func correlationID(r *http.Request) string {
	id, _ := r.Context().Value(correlationKey).(string)
	return id
}

func principal(r *http.Request) auth.Principal {
	p, _ := r.Context().Value(principalKey).(auth.Principal)
	return p
}

// require autentica (Bearer JWT) e exige o papel; role "" aceita provedor ou interno.
func (a *API) require(role string, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		scheme, token, ok := strings.Cut(h, " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="wallet"`)
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing or malformed bearer token")
			return
		}
		p, err := a.verifier.Verify(r.Context(), strings.TrimSpace(token))
		if err != nil {
			if errors.Is(err, auth.ErrIdPUnavailable) {
				w.Header().Set("Retry-After", "1")
				writeError(w, http.StatusServiceUnavailable, "IDENTITY_PROVIDER_UNAVAILABLE", "cannot validate credentials right now")
				return
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="wallet", error="invalid_token"`)
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid or expired token")
			return
		}
		allowed := (role == "" && (p.HasRole(auth.RoleProvider) || p.HasRole(auth.RoleInternal))) || (role != "" && p.HasRole(role))
		if !allowed {
			writeError(w, http.StatusForbidden, "FORBIDDEN", "insufficient permissions")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), principalKey, p)))
	})
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// fail traduz erros de aplicação em respostas distinguíveis.
func fail(w http.ResponseWriter, r *http.Request, err error) {
	var ve *wagering.ValidationError
	switch {
	case errors.As(err, &ve):
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", ve.Error())
	case errors.Is(err, app.ErrWalletNotFound):
		writeError(w, http.StatusNotFound, "WALLET_NOT_FOUND", "wallet not found")
	case errors.Is(err, app.ErrTransactionNotFound):
		writeError(w, http.StatusNotFound, "TRANSACTION_NOT_FOUND", "transaction not found")
	case errors.Is(err, app.ErrWalletAlreadyExists):
		writeError(w, http.StatusConflict, "WALLET_ALREADY_EXISTS", err.Error())
	case errors.Is(err, app.ErrIdempotencyConflict):
		writeError(w, http.StatusConflict, "IDEMPOTENCY_KEY_CONFLICT", err.Error())
	case errors.Is(err, app.ErrExternalTransactionConflict):
		writeError(w, http.StatusConflict, "EXTERNAL_TRANSACTION_CONFLICT", err.Error())
	case app.IsTransient(err):
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "temporarily unavailable, retry later")
	default:
		slog.Error("erro interno", "error", err, "correlationId", correlationID(r), "path", r.URL.Path)
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
	}
}

func (a *API) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := a.ready(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ---------------------------------------------------------------------------
// Carteiras (somente serviço interno)
// ---------------------------------------------------------------------------

type walletResponse struct {
	ID        string             `json:"id"`
	PlayerID  string             `json:"playerId"`
	Balance   wagering.MoneyData `json:"balance"`
	Version   int64              `json:"version"`
	CreatedAt string             `json:"createdAt,omitempty"`
	UpdatedAt string             `json:"updatedAt,omitempty"`
}

func toWalletResponse(w *wallet.Wallet, full bool) walletResponse {
	r := walletResponse{ID: w.ID(), PlayerID: w.PlayerID(), Balance: wagering.ToMoneyData(w.Balance()), Version: w.Version()}
	if full {
		r.CreatedAt, r.UpdatedAt = w.CreatedAt().UTC().Format(time.RFC3339Nano), w.UpdatedAt().UTC().Format(time.RFC3339Nano)
	}
	return r
}

func (a *API) handleCreateWallet(w http.ResponseWriter, r *http.Request) {
	var in struct {
		PlayerID       string             `json:"playerId"`
		InitialBalance wagering.MoneyData `json:"initialBalance"`
	}
	if !decode(w, r, &in) {
		return
	}
	m, err := money.Parse(in.InitialBalance.Amount, in.InitialBalance.Currency)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid initialBalance: "+err.Error())
		return
	}
	wl, err := a.svc.CreateWallet(r.Context(), in.PlayerID, m, correlationID(r))
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toWalletResponse(wl, false))
}

func (a *API) handleGetWallet(w http.ResponseWriter, r *http.Request) {
	wl, err := a.svc.GetWallet(r.Context(), r.PathValue("walletId"))
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toWalletResponse(wl, true))
}

type ledgerItem struct {
	ID            string             `json:"id"`
	TransactionID string             `json:"transactionId"`
	Direction     wallet.Direction   `json:"direction"`
	Money         wagering.MoneyData `json:"money"`
	BalanceBefore wagering.MoneyData `json:"balanceBefore"`
	BalanceAfter  wagering.MoneyData `json:"balanceAfter"`
	CreatedAt     string             `json:"createdAt"`
}

func encodeCursor(seq int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte("v1:" + strconv.FormatInt(seq, 10)))
}

func decodeCursor(c string) (int64, error) {
	if c == "" {
		return 0, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil || !strings.HasPrefix(string(b), "v1:") {
		return 0, errors.New("invalid cursor")
	}
	n, err := strconv.ParseInt(strings.TrimPrefix(string(b), "v1:"), 10, 64)
	if err != nil || n < 0 {
		return 0, errors.New("invalid cursor")
	}
	return n, nil
}

func (a *API) handleLedger(w http.ResponseWriter, r *http.Request) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "malformed query string")
		return
	}
	after, err := decodeCursor(q.Get("cursor"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid cursor")
		return
	}
	limit := 50
	if s := q.Get("limit"); s != "" {
		limit, err = strconv.Atoi(s)
		if err != nil || limit < 1 || limit > 200 {
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "limit must be between 1 and 200")
			return
		}
	}
	rows, err := a.svc.LedgerPage(r.Context(), r.PathValue("walletId"), after, limit+1)
	if err != nil {
		fail(w, r, err)
		return
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		next = encodeCursor(rows[len(rows)-1].Seq)
	}
	items := make([]ledgerItem, 0, len(rows))
	for _, row := range rows {
		m := row.Entry.Movement()
		items = append(items, ledgerItem{
			ID: row.Entry.ID(), TransactionID: row.Entry.TransactionID(), Direction: m.Direction,
			Money: wagering.ToMoneyData(m.Amount), BalanceBefore: wagering.ToMoneyData(m.Before),
			BalanceAfter: wagering.ToMoneyData(m.After), CreatedAt: row.Entry.CreatedAt().UTC().Format(time.RFC3339Nano),
		})
	}
	resp := map[string]any{"items": items}
	if next != "" {
		resp["nextCursor"] = next
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *API) handleReconcile(w http.ResponseWriter, r *http.Request) {
	rec, err := a.svc.Reconcile(r.Context(), r.PathValue("walletId"))
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"walletId": rec.WalletID, "storedBalance": wagering.ToMoneyData(rec.Stored),
		"calculatedBalance": wagering.ToMoneyData(rec.Calculated), "difference": wagering.ToMoneyData(rec.Difference),
		"consistent": rec.Consistent, "checkedEntries": rec.CheckedEntries,
	})
}

// ---------------------------------------------------------------------------
// Operações de apostas
// ---------------------------------------------------------------------------

// SubmitRequest é o corpo de POST /wagering/transactions.
type SubmitRequest struct {
	ProviderID                     string             `json:"providerId"`
	ExternalTransactionID          string             `json:"externalTransactionId"`
	PlayerID                       string             `json:"playerId"`
	WalletID                       string             `json:"walletId"`
	RoundID                        string             `json:"roundId"`
	GameID                         string             `json:"gameId"`
	Kind                           string             `json:"kind"`
	Money                          wagering.MoneyData `json:"money"`
	ReferenceExternalTransactionID string             `json:"referenceExternalTransactionId,omitempty"`
}

type txResult struct {
	TransactionID    string              `json:"transactionId"`
	Status           wagering.Status     `json:"status"`
	Balance          *wagering.MoneyData `json:"balance,omitempty"`
	FailureCode      string              `json:"failureCode,omitempty"`
	NextAttemptAt    string              `json:"nextAttemptAt,omitempty"`
	IdempotentReplay bool                `json:"idempotentReplay"`
}

func toResult(t *wagering.Transaction, replay bool) txResult {
	res := txResult{TransactionID: t.ID(), Status: t.Status(), FailureCode: string(t.FailureCode()), IdempotentReplay: replay}
	if b := t.ResultBalance(); b != nil {
		md := wagering.ToMoneyData(*b)
		res.Balance = &md
	}
	if n := t.NextAttemptAt(); n != nil && t.Status() == wagering.StatusPendingReference {
		res.NextAttemptAt = n.UTC().Format(time.RFC3339Nano)
	}
	return res
}

// statusFor: PROCESSED 200; PENDING_REFERENCE 202; REJECTED 422; FAILED 500.
func statusFor(s wagering.Status) int {
	switch s {
	case wagering.StatusProcessed:
		return http.StatusOK
	case wagering.StatusPendingReference, wagering.StatusPending:
		return http.StatusAccepted
	case wagering.StatusRejected:
		return http.StatusUnprocessableEntity
	}
	return http.StatusInternalServerError
}

func (a *API) handleSubmit(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		writeError(w, http.StatusBadRequest, "MISSING_IDEMPOTENCY_KEY", "the Idempotency-Key header is required")
		return
	}
	var in SubmitRequest
	if !decode(w, r, &in) {
		return
	}
	// a identidade autenticada determina o provedor autorizado
	if in.ProviderID != principal(r).ProviderID() {
		writeError(w, http.StatusForbidden, "PROVIDER_MISMATCH", "providerId does not match the authenticated provider")
		return
	}
	m, err := money.Parse(in.Money.Amount, in.Money.Currency)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid money: "+err.Error())
		return
	}
	res, err := a.svc.Process(r.Context(), wagering.Command{
		ProviderID: in.ProviderID, ExternalTransactionID: in.ExternalTransactionID, IdempotencyKey: key,
		PlayerID: in.PlayerID, WalletID: in.WalletID, RoundID: in.RoundID, GameID: in.GameID,
		Kind: wagering.Kind(in.Kind), Money: m, ReferenceExternalTransactionID: in.ReferenceExternalTransactionID,
		CorrelationID: correlationID(r),
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	t := res.Transaction
	slog.Info("operação processada", "correlationId", correlationID(r), "transactionId", t.ID(),
		"walletId", t.Command().WalletID, "providerId", t.Command().ProviderID, "kind", t.Kind(),
		"status", t.Status(), "failureCode", t.FailureCode(), "replay", res.Replay)
	writeJSON(w, statusFor(t.Status()), toResult(t, res.Replay))
}

type txDetail struct {
	txResult
	Kind                           wagering.Kind      `json:"kind"`
	WalletID                       string             `json:"walletId"`
	PlayerID                       string             `json:"playerId"`
	ProviderID                     string             `json:"providerId,omitempty"`
	ExternalTransactionID          string             `json:"externalTransactionId,omitempty"`
	RoundID                        string             `json:"roundId,omitempty"`
	GameID                         string             `json:"gameId,omitempty"`
	Money                          wagering.MoneyData `json:"money"`
	ReferenceExternalTransactionID string             `json:"referenceExternalTransactionId,omitempty"`
	Attempts                       int                `json:"attempts"`
	CreatedAt                      string             `json:"createdAt"`
	UpdatedAt                      string             `json:"updatedAt"`
}

func toDetail(t *wagering.Transaction) txDetail {
	c := t.Command()
	return txDetail{
		txResult: toResult(t, false), Kind: c.Kind, WalletID: c.WalletID, PlayerID: c.PlayerID,
		ProviderID: c.ProviderID, ExternalTransactionID: c.ExternalTransactionID, RoundID: c.RoundID, GameID: c.GameID,
		Money: wagering.ToMoneyData(c.Money), ReferenceExternalTransactionID: c.ReferenceExternalTransactionID,
		Attempts: t.Attempts(), CreatedAt: t.CreatedAt().UTC().Format(time.RFC3339Nano), UpdatedAt: t.UpdatedAt().UTC().Format(time.RFC3339Nano),
	}
}

// visible: o serviço interno enxerga tudo; um provedor só as suas (as de outros viram 404).
func visible(p auth.Principal, t *wagering.Transaction) bool {
	if p.HasRole(auth.RoleInternal) {
		return true
	}
	return t.Command().ProviderID != "" && t.Command().ProviderID == p.ProviderID()
}

func (a *API) handleGetTransaction(w http.ResponseWriter, r *http.Request) {
	t, err := a.svc.GetTransaction(r.Context(), r.PathValue("transactionId"))
	if err != nil {
		fail(w, r, err)
		return
	}
	if !visible(principal(r), t) {
		fail(w, r, app.ErrTransactionNotFound)
		return
	}
	writeJSON(w, http.StatusOK, toDetail(t))
}

func (a *API) handleGetProviderTransaction(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	providerID := r.PathValue("providerId")
	if !p.HasRole(auth.RoleInternal) && providerID != p.ProviderID() {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "cannot access another provider's transactions")
		return
	}
	t, err := a.svc.GetProviderTransaction(r.Context(), providerID, r.PathValue("externalTransactionId"))
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toDetail(t))
}
