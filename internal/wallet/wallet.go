package wallet

import (
	"time"

	"github.com/Cakez777/wallet-service/internal/money"
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
