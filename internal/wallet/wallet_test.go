package wallet

import (
	"errors"
	"testing"
	"time"

	"github.com/Cakez777/wallet-service/internal/money"
)

var now = time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

func brl(t *testing.T, s string) money.Money {
	t.Helper()
	m, err := money.Parse(s, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestNewWallet(t *testing.T) {
	w, err := New("w1", "p1", brl(t, "10.00"), now)
	if err != nil {
		t.Fatal(err)
	}
	if w.Version() != 1 || w.Balance().Amount() != "10.00" {
		t.Fatalf("estado inicial inesperado: v%d %s", w.Version(), w.Balance().Amount())
	}
	if _, err := New("", "p1", brl(t, "1.00"), now); !errors.Is(err, ErrInvalidWallet) {
		t.Errorf("id vazio deveria falhar, veio %v", err)
	}
	if _, err := New("w1", "p1", money.Money{}, now); !errors.Is(err, ErrInvalidWallet) {
		t.Errorf("Money não inicializado deveria falhar, veio %v", err)
	}
	neg, _ := money.FromCents(-1, "BRL")
	if _, err := New("w1", "p1", neg, now); !errors.Is(err, ErrInvalidWallet) {
		t.Errorf("saldo inicial negativo deveria falhar, veio %v", err)
	}
}

func TestApplyDebitCreditAndVersion(t *testing.T) {
	w, _ := New("w1", "p1", brl(t, "100.00"), now)

	m, err := w.Apply(Debit, brl(t, "80.00"), now)
	if err != nil {
		t.Fatal(err)
	}
	if m.Before.Amount() != "100.00" || m.After.Amount() != "20.00" || w.Version() != 2 {
		t.Fatalf("débito: %+v versão %d", m, w.Version())
	}
	if _, err := w.Apply(Credit, brl(t, "5.50"), now); err != nil {
		t.Fatal(err)
	}
	if w.Balance().Amount() != "25.50" || w.Version() != 3 {
		t.Fatalf("saldo %s versão %d", w.Balance().Amount(), w.Version())
	}
}

func TestApplyRejectsAndKeepsState(t *testing.T) {
	w, _ := New("w1", "p1", brl(t, "20.00"), now)
	usd, _ := money.Parse("1.00", "USD")

	cases := []struct {
		name string
		dir  Direction
		amt  money.Money
		want error
	}{
		{"saldo insuficiente", Debit, brl(t, "20.01"), ErrInsufficientFunds},
		{"moeda diferente", Credit, usd, ErrCurrencyMismatch},
		{"valor zero", Credit, brl(t, "0.00"), ErrInvalidAmount},
		{"direção inválida", Direction("X"), brl(t, "1.00"), ErrInvalidDirection},
	}
	for _, c := range cases {
		if _, err := w.Apply(c.dir, c.amt, now); !errors.Is(err, c.want) {
			t.Errorf("%s: esperado %v, veio %v", c.name, c.want, err)
		}
	}
	if w.Balance().Amount() != "20.00" || w.Version() != 1 {
		t.Errorf("estado mudou após rejeições: %s v%d", w.Balance().Amount(), w.Version())
	}
}

func TestRehydrateDoesNotApplyAnything(t *testing.T) {
	w, err := Rehydrate("w1", "p1", brl(t, "7.00"), 5, now, now)
	if err != nil {
		t.Fatal(err)
	}
	if w.Version() != 5 || w.Balance().Amount() != "7.00" {
		t.Fatalf("reidratação alterou o estado: v%d %s", w.Version(), w.Balance().Amount())
	}
	if _, err := Rehydrate("w1", "p1", brl(t, "7.00"), 0, now, now); err == nil {
		t.Error("versão 0 deveria falhar")
	}
}

func TestLedgerEntryValidatesArithmetic(t *testing.T) {
	ok := Movement{Direction: Debit, Amount: brl(t, "80.00"), Before: brl(t, "100.00"), After: brl(t, "20.00")}
	if _, err := NewLedgerEntry("e1", "w1", "t1", ok, now); err != nil {
		t.Fatalf("lançamento válido rejeitado: %v", err)
	}
	bad := Movement{Direction: Debit, Amount: brl(t, "80.00"), Before: brl(t, "100.00"), After: brl(t, "30.00")}
	if _, err := NewLedgerEntry("e1", "w1", "t1", bad, now); !errors.Is(err, ErrInvalidLedger) {
		t.Errorf("aritmética errada deveria falhar, veio %v", err)
	}
	if _, err := NewLedgerEntry("", "w1", "t1", ok, now); !errors.Is(err, ErrInvalidLedger) {
		t.Errorf("id vazio deveria falhar, veio %v", err)
	}
}
