package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

var (
	ErrInvalidMoney     = errors.New("invalid money")
	ErrCurrencyMismatch = errors.New("currency mismatch")
	ErrOverflow         = errors.New("numeric overflow")
)

// Money is immutable; its zero Go value is deliberately invalid.
type Money struct {
	cents    int64
	currency string
}

func validCurrency(c string) bool { return c == "BRL" || c == "USD" || c == "EUR" }
func FromCents(cents int64, currency string) (Money, error) {
	if !validCurrency(currency) {
		return Money{}, ErrInvalidMoney
	}
	return Money{cents: cents, currency: currency}, nil
}
func Zero(currency string) (Money, error) { return FromCents(0, currency) }

// ParseMoney accepts nonnegative external values with exactly two decimal places.
func ParseMoney(amount, currency string) (Money, error) {
	if !validCurrency(currency) {
		return Money{}, ErrInvalidMoney
	}
	parts := strings.Split(amount, ".")
	if len(parts) != 2 || len(parts[0]) == 0 || len(parts[1]) != 2 {
		return Money{}, ErrInvalidMoney
	}
	digits := parts[0] + parts[1]
	var cents int64
	for _, ch := range digits {
		if ch < '0' || ch > '9' {
			return Money{}, ErrInvalidMoney
		}
		digit := int64(ch - '0')
		if cents > (math.MaxInt64-digit)/10 {
			return Money{}, ErrOverflow
		}
		cents = cents*10 + digit
	}
	return FromCents(cents, currency)
}
func (m Money) Valid() bool      { return validCurrency(m.currency) }
func (m Money) Cents() int64     { return m.cents }
func (m Money) Currency() string { return m.currency }
func (m Money) compatible(n Money) error {
	if !m.Valid() || !n.Valid() {
		return ErrInvalidMoney
	}
	if m.currency != n.currency {
		return ErrCurrencyMismatch
	}
	return nil
}
func (m Money) Add(n Money) (Money, error) {
	if err := m.compatible(n); err != nil {
		return Money{}, err
	}
	if (n.cents > 0 && m.cents > math.MaxInt64-n.cents) || (n.cents < 0 && m.cents < math.MinInt64-n.cents) {
		return Money{}, ErrOverflow
	}
	return FromCents(m.cents+n.cents, m.currency)
}
func (m Money) Subtract(n Money) (Money, error) {
	if err := m.compatible(n); err != nil {
		return Money{}, err
	}
	if (n.cents > 0 && m.cents < math.MinInt64+n.cents) || (n.cents < 0 && m.cents > math.MaxInt64+n.cents) {
		return Money{}, ErrOverflow
	}
	return FromCents(m.cents-n.cents, m.currency)
}
func (m Money) Negate() (Money, error) {
	if !m.Valid() {
		return Money{}, ErrInvalidMoney
	}
	if m.cents == math.MinInt64 {
		return Money{}, ErrOverflow
	}
	return FromCents(-m.cents, m.currency)
}
func (m Money) Compare(n Money) (int, error) {
	if err := m.compatible(n); err != nil {
		return 0, err
	}
	if m.cents < n.cents {
		return -1, nil
	}
	if m.cents > n.cents {
		return 1, nil
	}
	return 0, nil
}
func (m Money) Amount() string {
	// Unsigned magnitude also handles MinInt64 without negating it.
	magnitude := uint64(m.cents)
	sign := ""
	if m.cents < 0 {
		magnitude = uint64(-(m.cents + 1)) + 1
		sign = "-"
	}
	return sign + strconv.FormatUint(magnitude/100, 10) + fmt.Sprintf(".%02d", magnitude%100)
}
func (m Money) MarshalJSON() ([]byte, error) {
	if !m.Valid() {
		return nil, ErrInvalidMoney
	}
	return json.Marshal(struct {
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
	}{m.Amount(), m.currency})
}
func (m *Money) UnmarshalJSON(data []byte) error {
	var wire struct {
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	parsed, err := ParseMoney(wire.Amount, wire.Currency)
	if err != nil {
		return err
	}
	*m = parsed
	return nil
}
