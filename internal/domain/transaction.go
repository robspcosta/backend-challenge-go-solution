package domain

import (
	"errors"
	"strings"
	"time"
)

var ErrInvalidTransaction = errors.New("invalid transaction")
var ErrInvalidTransition = errors.New("invalid state transition")

type Operation struct {
	ProviderID                     string `json:"providerId"`
	ExternalTransactionID          string `json:"externalTransactionId"`
	PlayerID                       string `json:"playerId"`
	WalletID                       string `json:"walletId"`
	RoundID                        string `json:"roundId"`
	GameID                         string `json:"gameId"`
	Kind                           string `json:"kind"`
	Money                          Money  `json:"money"`
	ReferenceExternalTransactionID string `json:"referenceExternalTransactionId,omitempty"`
}

func (o Operation) Validate() error {
	for _, value := range []string{o.ProviderID, o.ExternalTransactionID, o.PlayerID, o.WalletID, o.RoundID, o.GameID} {
		if value == "" || value != strings.TrimSpace(value) || len(value) > 256 {
			return ErrInvalidTransaction
		}
	}
	if !o.Money.Valid() || o.Money.Cents() < 0 {
		return ErrInvalidMoney
	}
	switch o.Kind {
	case "BET", "WIN", "REFUND", "ROLLBACK":
		if o.Money.Cents() == 0 {
			return ErrInvalidMovement
		}
	case "LOSS":
		if o.Money.Cents() != 0 {
			return ErrInvalidMovement
		}
	default:
		return ErrInvalidTransaction
	}
	if o.ReferenceExternalTransactionID != "" && o.ReferenceExternalTransactionID == o.ExternalTransactionID {
		return ErrInvalidTransaction
	}
	if (o.Kind == "REFUND" || o.Kind == "ROLLBACK") && o.ReferenceExternalTransactionID == "" {
		return ErrInvalidTransaction
	}
	if o.ReferenceExternalTransactionID != "" && (o.Kind != "WIN" && o.Kind != "REFUND" && o.Kind != "ROLLBACK" || o.ReferenceExternalTransactionID != strings.TrimSpace(o.ReferenceExternalTransactionID) || len(o.ReferenceExternalTransactionID) > 256) {
		return ErrInvalidTransaction
	}
	return nil
}

type Transaction struct {
	id, key              string
	operation            Operation
	hash                 [32]byte
	status, failureCode  string
	balance              Money
	createdAt, updatedAt time.Time
}

func NewTransaction(id, key string, o Operation, hash [32]byte, now time.Time) (*Transaction, error) {
	if id == "" || id != strings.TrimSpace(id) || key == "" || key != strings.TrimSpace(key) || len(key) > 256 || now.IsZero() {
		return nil, ErrInvalidTransaction
	}
	if err := o.Validate(); err != nil {
		return nil, err
	}
	return &Transaction{id: id, key: key, operation: o, hash: hash, status: "PENDING", createdAt: now.UTC(), updatedAt: now.UTC()}, nil
}
func (t *Transaction) Complete(balance Money, failureCode string, now time.Time) error {
	if t == nil || (t.status != "PENDING" && t.status != "PENDING_REFERENCE") {
		return ErrInvalidTransition
	}
	if failureCode != strings.TrimSpace(failureCode) || !balance.Valid() || balance.Cents() < 0 || balance.Currency() != t.operation.Money.Currency() || now.Before(t.createdAt) {
		return ErrInvalidTransaction
	}
	t.status = "PROCESSED"
	if failureCode != "" {
		t.status = "REJECTED"
	}
	t.failureCode = failureCode
	t.balance = balance
	t.updatedAt = now.UTC()
	return nil
}
func (t Transaction) ID() string           { return t.id }
func (t Transaction) Key() string          { return t.key }
func (t Transaction) Operation() Operation { return t.operation }
func (t Transaction) Hash() [32]byte       { return t.hash }
func (t Transaction) Status() string       { return t.status }
func (t Transaction) FailureCode() string  { return t.failureCode }
func (t Transaction) Balance() Money       { return t.balance }
func (t Transaction) CreatedAt() time.Time { return t.createdAt }
func (t Transaction) UpdatedAt() time.Time { return t.updatedAt }

func (t *Transaction) AwaitReference(balance Money, now time.Time) error {
	if t == nil || t.status != "PENDING" {
		return ErrInvalidTransition
	}
	if !balance.Valid() || balance.Cents() < 0 || balance.Currency() != t.operation.Money.Currency() || now.Before(t.createdAt) {
		return ErrInvalidTransaction
	}
	t.status = "PENDING_REFERENCE"
	t.balance = balance
	t.updatedAt = now.UTC()
	return nil
}

// RestorePending validates a saved operation without reapplying its movement.
func RestorePending(id, key string, o Operation, hash [32]byte, balance Money, created, updated time.Time) (*Transaction, error) {
	t, err := NewTransaction(id, key, o, hash, created)
	if err != nil {
		return nil, err
	}
	if err = t.AwaitReference(balance, updated); err != nil {
		return nil, err
	}
	return t, nil
}
