package domain

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
)

func TestParseMoney(t *testing.T) {
	for _, tc := range []struct {
		input string
		cents int64
		want  error
	}{
		{"0.00", 0, nil}, {"25.00", 2500, nil}, {"00025.00", 2500, nil}, {"92233720368547758.07", math.MaxInt64, nil},
		{"92233720368547758.08", 0, ErrOverflow}, {"999999999999999999999.00", 0, ErrOverflow},
		{"", 0, ErrInvalidMoney}, {"-1.00", 0, ErrInvalidMoney}, {"+1.00", 0, ErrInvalidMoney},
		{"1", 0, ErrInvalidMoney}, {"1.0", 0, ErrInvalidMoney}, {"1.000", 0, ErrInvalidMoney},
		{"NaN", 0, ErrInvalidMoney}, {"Infinity", 0, ErrInvalidMoney}, {"1e2", 0, ErrInvalidMoney},
		{" 1.00", 0, ErrInvalidMoney}, {".00", 0, ErrInvalidMoney}, {"1,00", 0, ErrInvalidMoney},
	} {
		t.Run(tc.input, func(t *testing.T) {
			m, err := ParseMoney(tc.input, "BRL")
			if !errors.Is(err, tc.want) {
				t.Fatalf("error=%v want=%v", err, tc.want)
			}
			if err == nil && m.Cents() != tc.cents {
				t.Fatal(m.Cents())
			}
		})
	}
	if _, err := ParseMoney("1.00", "ZZZ"); !errors.Is(err, ErrInvalidMoney) {
		t.Fatal(err)
	}
}
func money(c int64) Money { m, _ := FromCents(c, "BRL"); return m }
func TestMoneyArithmetic(t *testing.T) {
	for _, tc := range []struct {
		name     string
		a, b     int64
		subtract bool
		want     int64
		err      error
	}{
		{"add", 100, 20, false, 120, nil}, {"subtract", 100, 120, true, -20, nil},
		{"add max", math.MaxInt64, 1, false, 0, ErrOverflow}, {"add min", math.MinInt64, -1, false, 0, ErrOverflow},
		{"sub min", math.MinInt64, 1, true, 0, ErrOverflow}, {"sub max", math.MaxInt64, -1, true, 0, ErrOverflow},
		{"min minus min", math.MinInt64, math.MinInt64, true, 0, nil},
		{"max plus min", math.MaxInt64, math.MinInt64, false, -1, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got Money
			var err error
			if tc.subtract {
				got, err = money(tc.a).Subtract(money(tc.b))
			} else {
				got, err = money(tc.a).Add(money(tc.b))
			}
			if !errors.Is(err, tc.err) || (err == nil && got.Cents() != tc.want) {
				t.Fatalf("%v %v", got, err)
			}
		})
	}
	if _, err := money(math.MinInt64).Negate(); !errors.Is(err, ErrOverflow) {
		t.Fatal(err)
	}
	if got, err := money(-20).Negate(); err != nil || got.Cents() != 20 {
		t.Fatal(got, err)
	}
	usd, _ := FromCents(1, "USD")
	if _, err := money(1).Add(usd); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatal(err)
	}
	if _, err := money(1).Subtract(usd); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatal(err)
	}
	if _, err := money(1).Compare(usd); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatal(err)
	}
	if cmp, err := money(1).Compare(money(2)); err != nil || cmp != -1 {
		t.Fatal(cmp, err)
	}
	var invalid Money
	if _, err := invalid.Add(money(0)); !errors.Is(err, ErrInvalidMoney) {
		t.Fatal(err)
	}
	if _, err := invalid.Negate(); !errors.Is(err, ErrInvalidMoney) {
		t.Fatal(err)
	}
}
func TestMoneyJSON(t *testing.T) {
	for _, tc := range []struct {
		c      int64
		amount string
	}{{0, "0.00"}, {1, "0.01"}, {-1, "-0.01"}, {math.MaxInt64, "92233720368547758.07"}, {math.MinInt64, "-92233720368547758.08"}} {
		m := money(tc.c)
		if m.Amount() != tc.amount {
			t.Fatal(m.Amount())
		}
		data, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		var wire map[string]string
		if err = json.Unmarshal(data, &wire); err != nil || wire["amount"] != tc.amount {
			t.Fatal(string(data), err)
		}
	}
	var m Money
	if err := json.Unmarshal([]byte(`{"amount":"25.00","currency":"BRL"}`), &m); err != nil || m.Cents() != 2500 {
		t.Fatal(m, err)
	}
	for _, data := range []string{`{"amount":25,"currency":"BRL"}`, `{"amount":"-1.00","currency":"BRL"}`, `null`, `{}`} {
		if err := json.Unmarshal([]byte(data), &m); err == nil {
			t.Fatal(data)
		}
	}
	if _, err := json.Marshal(Money{}); err == nil {
		t.Fatal("zero value accepted")
	}
}
