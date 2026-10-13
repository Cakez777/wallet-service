package app

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"net"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrWalletNotFound              = errors.New("wallet not found")
	ErrWalletAlreadyExists         = errors.New("a wallet already exists for this player and currency")
	ErrTransactionNotFound         = errors.New("transaction not found")
	ErrIdempotencyConflict         = errors.New("idempotency key already used with a different payload")
	ErrExternalTransactionConflict = errors.New("external transaction already registered with another idempotency key")
)

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func isUniqueViolation(err error) bool { return pgCode(err) == "23505" }

// isRetryable: deadlock ou falha de serialização; a operação inteira pode ser repetida.
func isRetryable(err error) bool {
	c := pgCode(err)
	return c == "40001" || c == "40P01"
}

// IsTransient informa se o erro indica indisponibilidade temporária da infraestrutura
// (PostgreSQL fora, conexão caída, timeout). Nada foi persistido; o cliente pode repetir.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
		errors.Is(err, driver.ErrBadConn) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	c := pgCode(err)
	return strings.HasPrefix(c, "08") || strings.HasPrefix(c, "53") || strings.HasPrefix(c, "57") || isRetryable(err)
}
