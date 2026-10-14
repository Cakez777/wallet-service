package app

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

type fakeNetErr struct{}

func (fakeNetErr) Error() string   { return "net" }
func (fakeNetErr) Timeout() bool   { return true }
func (fakeNetErr) Temporary() bool { return true }

// IsTransient decide entre "repita depois" (503 / retorno à fila) e erro definitivo.
func TestIsTransientClassification(t *testing.T) {
	pg := func(code string) error { return &pgconn.PgError{Code: code} }
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"deadline", context.DeadlineExceeded, true},
		{"cancelado", context.Canceled, true},
		{"conexão ruim", driver.ErrBadConn, true},
		{"EOF", io.EOF, true},
		{"EOF inesperado", io.ErrUnexpectedEOF, true},
		{"erro de rede", fakeNetErr{}, true},
		{"embrulhado", fmt.Errorf("x: %w", io.EOF), true},
		{"PG conexão (08)", pg("08006"), true},
		{"PG recursos (53)", pg("53300"), true},
		{"PG desligando (57)", pg("57P01"), true},
		{"PG deadlock", pg("40P01"), true},
		{"PG serialização", pg("40001"), true},
		{"PG unicidade", pg("23505"), false},
		{"PG check", pg("23514"), false},
		{"erro comum", errors.New("boom"), false},
	}
	for _, c := range cases {
		if got := IsTransient(c.err); got != c.want {
			t.Errorf("%s: IsTransient = %v, esperado %v", c.name, got, c.want)
		}
	}
}
