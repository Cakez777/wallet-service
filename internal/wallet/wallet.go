package wallet

import (
	"time"

	"github.com/Cakez777/wallet-service/internal/money"
)

// Constantes padronizadas para os tipos de transação do domínio de iGaming/Apostas
const (
	TxTypeDeposit  = "DEPOSIT"
	TxTypeWithdraw = "WITHDRAW"
	TxTypeBet      = "BET"
	TxTypeWin      = "WIN"
	TxTypeLoss     = "LOSS"
	TxTypeRefund   = "REFUND"
	TxTypeRollback = "ROLLBACK"
)

// Wallet representa a carteira de um jogador.
type Wallet struct {
	ID        string      `json:"id"`
	PlayerID  string      `json:"player_id"`
	Balance   money.Money `json:"balance"`
	Version   int64       `json:"version"`
	CreatedAt time.Time   `json:"created_at"`
	UpdatedAt time.Time   `json:"updated_at"`
}
