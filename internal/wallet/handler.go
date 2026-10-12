package wallet

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type CreateWalletInput struct {
	PlayerID string `json:"player_id"`
	Currency string `json:"currency"`
}

type TransactionInput struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

type Handler struct {
	service *Service
}

// NewHandler cria uma nova instância do handler HTTP de wallets.
func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// HandleCreateWallet lida com as requisições POST /wallets.
func (h *Handler) HandleCreateWallet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Método não permitido", http.StatusMethodNotAllowed)
		return
	}

	var input CreateWalletInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "Corpo da requisição inválido", http.StatusBadRequest)
		return
	}

	wallet, err := h.service.CreateWallet(r.Context(), input.PlayerID, input.Currency)
	if err != nil {
		http.Error(w, fmt.Sprint(err), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(wallet)
}

// extractWalletID extrai o ID da carteira da URL (esperando formato /wallets/{id}/deposit ou /wallets/{id}/withdraw)
func extractWalletID(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) >= 2 && parts[0] == "wallets" {
		return parts[1]
	}
	return ""
}

// HandleDeposit lida com POST /wallets/{id}/deposit
func (h *Handler) HandleDeposit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Método não permitido", http.StatusMethodNotAllowed)
		return
	}

	walletID := extractWalletID(r.URL.Path)
	if walletID == "" {
		http.Error(w, "ID da carteira inválido", http.StatusBadRequest)
		return
	}

	var input TransactionInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "Corpo da requisição inválido", http.StatusBadRequest)
		return
	}

	idempotencyKey := r.Header.Get("X-Idempotency-Key")

	wallet, err := h.service.Deposit(r.Context(), walletID, input.Amount, input.Currency, idempotencyKey)
	if err != nil {
		http.Error(w, fmt.Sprint(err), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(wallet)
}

// HandleWithdraw lida com POST /wallets/{id}/withdraw
func (h *Handler) HandleWithdraw(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Método não permitido", http.StatusMethodNotAllowed)
		return
	}

	walletID := extractWalletID(r.URL.Path)
	if walletID == "" {
		http.Error(w, "ID da carteira inválido", http.StatusBadRequest)
		return
	}

	var input TransactionInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "Corpo da requisição inválido", http.StatusBadRequest)
		return
	}

	idempotencyKey := r.Header.Get("X-Idempotency-Key")

	wallet, err := h.service.Withdraw(r.Context(), walletID, input.Amount, input.Currency, idempotencyKey)
	if err != nil {
		http.Error(w, fmt.Sprint(err), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(wallet)
}
