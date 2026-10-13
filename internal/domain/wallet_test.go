package domain

import (
	"errors"
	"math"
	"testing"
	"time"
)

func TestWalletTransitions(t *testing.T) {
	now := time.Now().UTC()
	w, err := NewWallet("wallet", "player", money(10000), now)
	if err != nil {
		t.Fatal(err)
	}
	if w.Version() != 1 {
		t.Fatal(w.Version())
	}
	if err = w.Debit(money(8000), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if w.Balance().Cents() != 2000 || w.Version() != 2 {
		t.Fatal(w)
	}
	before := *w
	if err = w.Debit(money(8000), now.Add(2*time.Second)); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatal(err)
	}
	if *w != before {
		t.Fatal("rejection changed state")
	}
	if err = w.Credit(money(3000), now.Add(3*time.Second)); err != nil || w.Balance().Cents() != 5000 || w.Version() != 3 {
		t.Fatal(w, err)
	}
	restored, err := RestoreWallet(w.ID(), w.PlayerID(), w.Balance(), w.Version(), w.CreatedAt(), w.UpdatedAt())
	if err != nil || *restored != *w {
		t.Fatal(restored, err)
	}
}
func TestWalletInvalidOperations(t *testing.T) {
	now := time.Now().UTC()
	usd, _ := FromCents(1, "USD")
	for _, tc := range []struct {
		name    string
		balance Money
		amount  Money
		version int64
		at      time.Time
		want    error
	}{
		{"zero", money(100), money(0), 1, now, ErrInvalidMovement},
		{"negative", money(100), money(-1), 1, now, ErrInvalidMovement},
		{"currency", money(100), usd, 1, now, ErrCurrencyMismatch},
		{"invalid money", money(100), Money{}, 1, now, ErrInvalidMoney},
		{"balance overflow", money(math.MaxInt64), money(1), 1, now, ErrOverflow},
		{"version overflow", money(100), money(1), math.MaxInt64, now, ErrOverflow},
		{"time backwards", money(100), money(1), 1, now.Add(-time.Second), ErrInvalidWallet},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, err := RestoreWallet("w", "p", tc.balance, tc.version, now, now)
			if err != nil {
				t.Fatal(err)
			}
			before := *w
			if err = w.Credit(tc.amount, tc.at); !errors.Is(err, tc.want) {
				t.Fatal(err)
			}
			if *w != before {
				t.Fatal("mutation on error")
			}
		})
	}
	var invalid Wallet
	if err := invalid.Debit(money(1), now); !errors.Is(err, ErrInvalidWallet) {
		t.Fatal(err)
	}
	for _, balance := range []Money{Money{}, money(-1)} {
		if _, err := NewWallet("w", "p", balance, now); !errors.Is(err, ErrInvalidWallet) {
			t.Fatal(err)
		}
	}
	if _, err := NewWallet("", "p", money(0), now); !errors.Is(err, ErrInvalidWallet) {
		t.Fatal(err)
	}
	if _, err := NewWallet("w", "p", money(0), time.Time{}); !errors.Is(err, ErrInvalidWallet) {
		t.Fatal(err)
	}
	if _, err := RestoreWallet("w", "p", money(0), 0, now, now); !errors.Is(err, ErrInvalidWallet) {
		t.Fatal(err)
	}
}
