// Package wagering contém o domínio de apostas: comandos, transações,
// máquina de estados, regras dos cinco tipos externos e eventos.
// É domínio puro: não conhece HTTP, SQL, SQS nem Fx.
package wagering

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/Cakez777/wallet-service/internal/money"
)

// Kind é o tipo da operação.
type Kind string

const (
	KindOpening  Kind = "OPENING" // reservado à abertura interna de carteira
	KindBet      Kind = "BET"
	KindWin      Kind = "WIN"
	KindLoss     Kind = "LOSS"
	KindRefund   Kind = "REFUND"
	KindRollback Kind = "ROLLBACK"
)

// ExternalKinds são os tipos aceitos por HTTP e SQS.
func (k Kind) IsExternal() bool {
	switch k {
	case KindBet, KindWin, KindLoss, KindRefund, KindRollback:
		return true
	}
	return false
}

// ValidationError é uma entrada corrigível: nada é persistido.
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string { return fmt.Sprintf("invalid %s: %s", e.Field, e.Reason) }

func invalid(field, reason string) error { return &ValidationError{Field: field, Reason: reason} }

// IsValidationError informa se err (ou um erro embrulhado) é ValidationError.
func IsValidationError(err error) bool {
	var v *ValidationError
	return errors.As(err, &v)
}

const maxFieldLen = 255

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// IsUUID valida o formato textual de um UUID.
func IsUUID(s string) bool { return uuidRe.MatchString(s) }

// Command é uma operação externa recebida por HTTP ou SQS, já convertida em
// tipos de domínio. CorrelationID é metadado de transporte e não entra no hash.
type Command struct {
	ProviderID                     string
	ExternalTransactionID          string
	IdempotencyKey                 string
	PlayerID                       string
	WalletID                       string
	RoundID                        string
	GameID                         string
	Kind                           Kind
	Money                          money.Money
	ReferenceExternalTransactionID string
	CorrelationID                  string
}

func checkText(field, v string) error {
	if v == "" {
		return invalid(field, "is required")
	}
	if len(v) > maxFieldLen {
		return invalid(field, fmt.Sprintf("exceeds %d characters", maxFieldLen))
	}
	return nil
}

// Validate aplica as regras de forma de entrada, independentes do estado do banco.
func (c Command) Validate() error {
	for _, f := range []struct{ name, value string }{
		{"providerId", c.ProviderID},
		{"externalTransactionId", c.ExternalTransactionID},
		{"idempotencyKey", c.IdempotencyKey},
		{"roundId", c.RoundID},
		{"gameId", c.GameID},
	} {
		if err := checkText(f.name, f.value); err != nil {
			return err
		}
	}
	if !IsUUID(c.PlayerID) {
		return invalid("playerId", "must be a UUID")
	}
	if !IsUUID(c.WalletID) {
		return invalid("walletId", "must be a UUID")
	}
	if c.Kind == KindOpening {
		return invalid("kind", "OPENING is reserved for internal wallet opening")
	}
	if !c.Kind.IsExternal() {
		return invalid("kind", fmt.Sprintf("%q is not one of BET, WIN, LOSS, REFUND, ROLLBACK", string(c.Kind)))
	}
	if c.Money.Currency() == "" {
		return invalid("money", "is required")
	}
	switch c.Kind {
	case KindLoss:
		if !c.Money.IsZero() {
			return invalid("money.amount", "must be 0.00 for LOSS")
		}
	default:
		if !c.Money.IsPositive() {
			return invalid("money.amount", fmt.Sprintf("must be greater than zero for %s", c.Kind))
		}
	}
	if len(c.ReferenceExternalTransactionID) > maxFieldLen {
		return invalid("referenceExternalTransactionId", fmt.Sprintf("exceeds %d characters", maxFieldLen))
	}
	switch c.Kind {
	case KindRefund, KindRollback:
		if c.ReferenceExternalTransactionID == "" {
			return invalid("referenceExternalTransactionId", fmt.Sprintf("is required for %s", c.Kind))
		}
	case KindBet:
		if c.ReferenceExternalTransactionID != "" {
			return invalid("referenceExternalTransactionId", "is not allowed for BET")
		}
	}
	if c.ReferenceExternalTransactionID != "" && c.ReferenceExternalTransactionID == c.ExternalTransactionID {
		return invalid("referenceExternalTransactionId", "cannot reference itself")
	}
	return nil
}

// PayloadHash devolve o SHA-256 (hex) de um JSON canônico dos campos de negócio.
//
// Campos: providerId, externalTransactionId, playerId, walletId, roundId, gameId,
// kind, money{amount,currency} e referenceExternalTransactionId ("" quando ausente).
// Ficam de fora: idempotencyKey e metadados de transporte (correlationId, messageId).
// As chaves são ordenadas (json.Marshal ordena maps) e o valor monetário já é canônico
// ("25.00"), pois Money.Parse só aceita escala exata de duas casas. HTTP e SQS montam o
// mesmo Command, então produzem o mesmo hash para a mesma operação.
func (c Command) PayloadHash() string {
	canonical := map[string]any{
		"providerId":                     c.ProviderID,
		"externalTransactionId":          c.ExternalTransactionID,
		"playerId":                       c.PlayerID,
		"walletId":                       c.WalletID,
		"roundId":                        c.RoundID,
		"gameId":                         c.GameID,
		"kind":                           string(c.Kind),
		"money":                          map[string]string{"amount": c.Money.Amount(), "currency": c.Money.Currency()},
		"referenceExternalTransactionId": c.ReferenceExternalTransactionID,
	}
	b, _ := json.Marshal(canonical) // maps de strings: não falha
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
