package money

import (
	"errors"
	"math"
	"testing"
)

func TestParseValid(t *testing.T) {
	cases := []struct {
		in    string
		cents int64
	}{
		{"0.00", 0},
		{"0.01", 1},
		{"25.00", 2500},
		{"1000.00", 100000},
	}
	for _, c := range cases {
		m, err := Parse(c.in, "BRL")
		if err != nil {
			t.Fatalf("Parse(%q): erro inesperado: %v", c.in, err)
		}
		if m.Cents() != c.cents {
			t.Errorf("Parse(%q) = %d centavos, esperado %d", c.in, m.Cents(), c.cents)
		}
		if m.Amount() != c.in {
			t.Errorf("Amount() = %q, esperado %q", m.Amount(), c.in)
		}
	}
}

func TestParseInvalid(t *testing.T) {
	inputs := []string{
		"", "25", "25.0", "25.000", "-25.00", "+25.00", "abc",
		"1e3", "NaN", "Infinity", " 25.00", "25,00",
		"9223372036854775807.00", // overflow
	}
	for _, in := range inputs {
		if _, err := Parse(in, "BRL"); err == nil {
			t.Errorf("Parse(%q) deveria falhar", in)
		}
	}
}

func TestInvalidCurrency(t *testing.T) {
	for _, cur := range []string{"", "br", "BRLL", "123", "ZZZ", "XXX"} {
		if _, err := Parse("1.00", cur); !errors.Is(err, ErrInvalidCurrency) {
			t.Errorf("moeda %q deveria dar ErrInvalidCurrency, veio %v", cur, err)
		}
	}
}

func TestAddSub(t *testing.T) {
	a, _ := Parse("100.00", "BRL")
	b, _ := Parse("80.00", "BRL")

	diff, err := a.Sub(b)
	if err != nil || diff.Amount() != "20.00" {
		t.Fatalf("100.00 - 80.00 = %v, %v", diff.Amount(), err)
	}
	sum, err := a.Add(b)
	if err != nil || sum.Amount() != "180.00" {
		t.Fatalf("100.00 + 80.00 = %v, %v", sum.Amount(), err)
	}
	neg, _ := b.Sub(a)
	if neg.Amount() != "-20.00" {
		t.Errorf("80.00 - 100.00 = %q, esperado -20.00", neg.Amount())
	}
}

func TestCurrencyMismatch(t *testing.T) {
	brl, _ := Parse("10.00", "BRL")
	usd, _ := Parse("10.00", "USD")
	if _, err := brl.Add(usd); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("Add: esperado ErrCurrencyMismatch, veio %v", err)
	}
	if _, err := brl.Sub(usd); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("Sub: esperado ErrCurrencyMismatch, veio %v", err)
	}
	if _, err := brl.Cmp(usd); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("Cmp: esperado ErrCurrencyMismatch, veio %v", err)
	}
}

func TestOverflow(t *testing.T) {
	max, _ := FromCents(math.MaxInt64, "BRL")
	one, _ := FromCents(1, "BRL")
	if _, err := max.Add(one); !errors.Is(err, ErrOverflow) {
		t.Errorf("Add overflow: veio %v", err)
	}
	min, _ := FromCents(math.MinInt64, "BRL")
	if _, err := min.Negate(); !errors.Is(err, ErrOverflow) {
		t.Errorf("Negate overflow: veio %v", err)
	}
	if _, err := one.Sub(min); !errors.Is(err, ErrOverflow) {
		t.Errorf("Sub overflow: veio %v", err)
	}
}
