package money

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

var (
	ErrInvalidAmount    = errors.New("invalid amount")
	ErrInvalidCurrency  = errors.New("invalid currency")
	ErrCurrencyMismatch = errors.New("currency mismatch")
	ErrOverflow         = errors.New("amount overflow")
)

var (
	amountRe   = regexp.MustCompile(`^[0-9]+\.[0-9]{2}$`)
	currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)
)

// Money é um value object imutável: valor em centavos (int64) + moeda.
type Money struct {
	cents    int64
	currency string
}

func validCurrency(currency string) error {
	if !currencyRe.MatchString(currency) {
		return fmt.Errorf("%w: %q", ErrInvalidCurrency, currency)
	}
	return nil
}

// Parse cria um Money a partir de uma string decimal com exatamente duas casas ("25.00").
// Rejeita vazio, sinal, notação científica, NaN, Infinity, escala errada e overflow.
func Parse(amount, currency string) (Money, error) {
	if err := validCurrency(currency); err != nil {
		return Money{}, err
	}
	if !amountRe.MatchString(amount) {
		return Money{}, fmt.Errorf("%w: %q", ErrInvalidAmount, amount)
	}
	// "25.00" -> "2500": tirar o ponto já dá os centavos.
	cents, err := strconv.ParseInt(strings.Replace(amount, ".", "", 1), 10, 64)
	if err != nil {
		return Money{}, fmt.Errorf("%w: %q", ErrOverflow, amount)
	}
	return Money{cents: cents, currency: currency}, nil
}

// FromCents cria um Money a partir de centavos (ex.: lido do banco).
func FromCents(cents int64, currency string) (Money, error) {
	if err := validCurrency(currency); err != nil {
		return Money{}, err
	}
	return Money{cents: cents, currency: currency}, nil
}

// Zero devolve o valor zero de uma moeda.
func Zero(currency string) (Money, error) {
	return FromCents(0, currency)
}

func (m Money) Cents() int64     { return m.cents }
func (m Money) Currency() string { return m.currency }
func (m Money) IsZero() bool     { return m.cents == 0 }
func (m Money) IsPositive() bool { return m.cents > 0 }
func (m Money) IsNegative() bool { return m.cents < 0 }

// Amount devolve a string decimal com duas casas, ex.: "25.00".
func (m Money) Amount() string {
	sign := ""
	var u uint64
	if m.cents < 0 {
		sign = "-"
		u = uint64(-(m.cents + 1)) + 1 // seguro mesmo para MinInt64
	} else {
		u = uint64(m.cents)
	}
	return fmt.Sprintf("%s%d.%02d", sign, u/100, u%100)
}

func (m Money) checkCurrency(o Money) error {
	if m.currency != o.currency {
		return fmt.Errorf("%w: %s vs %s", ErrCurrencyMismatch, m.currency, o.currency)
	}
	return nil
}

func (m Money) Add(o Money) (Money, error) {
	if err := m.checkCurrency(o); err != nil {
		return Money{}, err
	}
	if (o.cents > 0 && m.cents > math.MaxInt64-o.cents) ||
		(o.cents < 0 && m.cents < math.MinInt64-o.cents) {
		return Money{}, ErrOverflow
	}
	return Money{cents: m.cents + o.cents, currency: m.currency}, nil
}

func (m Money) Negate() (Money, error) {
	if m.cents == math.MinInt64 {
		return Money{}, ErrOverflow
	}
	return Money{cents: -m.cents, currency: m.currency}, nil
}

func (m Money) Sub(o Money) (Money, error) {
	if err := m.checkCurrency(o); err != nil {
		return Money{}, err
	}
	neg, err := o.Negate()
	if err != nil {
		return Money{}, err
	}
	return m.Add(neg)
}

// Cmp devolve -1, 0 ou 1.
func (m Money) Cmp(o Money) (int, error) {
	if err := m.checkCurrency(o); err != nil {
		return 0, err
	}
	switch {
	case m.cents < o.cents:
		return -1, nil
	case m.cents > o.cents:
		return 1, nil
	}
	return 0, nil
}
