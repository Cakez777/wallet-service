package wallet

import (
	"encoding/json"
	"net/http"
	"strings"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// TransactionInput define o payload padrão para as requisições de wagering
type TransactionInput struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// HandleBet processa POST /wallets/{id}/bet
func (h *Handler) HandleBet(w http.ResponseWriter, r *http.Request) {
	h.handleWager(w, r, TxTypeBet)
}

// HandleWin processa POST /wallets/{id}/win
func (h *Handler) HandleWin(w http.ResponseWriter, r *http.Request) {
	h.handleWager(w, r, TxTypeWin)
}

// HandleLoss processa POST /wallets/{id}/loss
func (h *Handler) HandleLoss(w http.ResponseWriter, r *http.Request) {
	h.handleWager(w, r, TxTypeLoss)
}

// HandleRefund processa POST /wallets/{id}/refund
func (h *Handler) HandleRefund(w http.ResponseWriter, r *http.Request) {
	h.handleWager(w, r, TxTypeRefund)
}

// HandleRollback processa POST /wallets/{id}/rollback
func (h *Handler) HandleRollback(w http.ResponseWriter, r *http.Request) {
	h.handleWager(w, r, TxTypeRollback)
}

// handleWager centraliza a lógica HTTP para qualquer operação de wagering
func (h *Handler) handleWager(w http.ResponseWriter, r *http.Request, txType string) {
	if r.Method != http.MethodPost {
		http.Error(w, "Método não permitido", http.StatusMethodNotAllowed)
		return
	}

	walletID := h.extractWalletID(r.URL.Path)
	if walletID == "" {
		http.Error(w, "ID da carteira inválido", http.StatusBadRequest)
		return
	}

	idempotencyKey := r.Header.Get("X-Idempotency-Key")

	var input TransactionInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "Corpo da requisição inválido", http.StatusBadRequest)
		return
	}

	// Executa a operação unificada no service
	response, err := h.service.ProcessWager(
		r.Context(),
		walletID,
		input.Amount,
		input.Currency,
		txType,
		idempotencyKey,
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(response)
}

// Helper simples para extrair o ID da URL (ex: /wallets/{id}/bet)
func (h *Handler) extractWalletID(path string) string {
	parts := strings.Split(path, "/")
	for i, part := range parts {
		if part == "wallets" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

// HandleCreateWallet processa POST /wallets
func (h *Handler) HandleCreateWallet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Método não permitido", http.StatusMethodNotAllowed)
		return
	}

	var input struct {
		PlayerID string `json:"player_id"`
		Currency string `json:"currency"`
	}

	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "Corpo da requisição inválido", http.StatusBadRequest)
		return
	}

	wallet, err := h.service.CreateWallet(r.Context(), input.PlayerID, input.Currency)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(wallet)
}

// HandleDeposit processa POST /wallets/{id}/deposit
func (h *Handler) HandleDeposit(w http.ResponseWriter, r *http.Request) {
	h.handleWager(w, r, TxTypeDeposit)
}

// HandleWithdraw processa POST /wallets/{id}/withdraw
func (h *Handler) HandleWithdraw(w http.ResponseWriter, r *http.Request) {
	h.handleWager(w, r, TxTypeWithdraw)
}
