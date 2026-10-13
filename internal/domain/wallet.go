package domain

import (
	"errors"
	"math"
	"strings"
	"time"
)

var (
	ErrInvalidWallet     = errors.New("invalid wallet")
	ErrInvalidMovement   = errors.New("movement must be positive")
	ErrInsufficientFunds = errors.New("insufficient funds")
)

type Wallet struct {
	id, playerID         string
	balance              Money
	version              int64
	createdAt, updatedAt time.Time
}

func NewWallet(id, playerID string, balance Money, now time.Time) (*Wallet, error) {
	return RestoreWallet(id, playerID, balance, 1, now, now)
}

// RestoreWallet validates a snapshot without replaying any movement.
func RestoreWallet(id, playerID string, balance Money, version int64, createdAt, updatedAt time.Time) (*Wallet, error) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(playerID) == "" || !balance.Valid() || balance.Cents() < 0 || version < 1 || createdAt.IsZero() || updatedAt.Before(createdAt) {
		return nil, ErrInvalidWallet
	}
	return &Wallet{id: id, playerID: playerID, balance: balance, version: version, createdAt: createdAt.UTC(), updatedAt: updatedAt.UTC()}, nil
}
func (w Wallet) ID() string                                { return w.id }
func (w Wallet) PlayerID() string                          { return w.playerID }
func (w Wallet) Balance() Money                            { return w.balance }
func (w Wallet) Version() int64                            { return w.version }
func (w Wallet) CreatedAt() time.Time                      { return w.createdAt }
func (w Wallet) UpdatedAt() time.Time                      { return w.updatedAt }
func (w *Wallet) Credit(amount Money, now time.Time) error { return w.change(amount, now, false) }
func (w *Wallet) Debit(amount Money, now time.Time) error  { return w.change(amount, now, true) }
func (w *Wallet) change(amount Money, now time.Time, debit bool) error {
	if w == nil || w.id == "" || !w.balance.Valid() || w.version < 1 {
		return ErrInvalidWallet
	}
	if !amount.Valid() {
		return ErrInvalidMoney
	}
	if amount.Currency() != w.balance.Currency() {
		return ErrCurrencyMismatch
	}
	if amount.Cents() <= 0 {
		return ErrInvalidMovement
	}
	if now.IsZero() || now.Before(w.updatedAt) {
		return ErrInvalidWallet
	}
	if w.version == math.MaxInt64 {
		return ErrOverflow
	}
	var next Money
	var err error
	if debit {
		next, err = w.balance.Subtract(amount)
	} else {
		next, err = w.balance.Add(amount)
	}
	if err != nil {
		return err
	}
	if next.Cents() < 0 {
		return ErrInsufficientFunds
	}
	w.balance = next
	w.version++
	w.updatedAt = now.UTC()
	return nil
}
