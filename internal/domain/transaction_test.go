package domain

import (
	"errors"
	"testing"
	"time"
)

func operation(kind string, cents int64) Operation {
	return Operation{ProviderID: "provider-a", ExternalTransactionID: "external-1", PlayerID: "player", WalletID: "wallet", RoundID: "round", GameID: "game", Kind: kind, Money: money(cents)}
}
func TestOperationRules(t *testing.T) {
	for _, tc := range []struct {
		kind   string
		amount int64
		valid  bool
	}{{"BET", 1, true}, {"BET", 0, false}, {"WIN", 1, true}, {"WIN", 0, false}, {"LOSS", 0, true}, {"LOSS", 1, false}, {"LOSS", -1, false}, {"OPENING", 1, false}, {"REFUND", 1, false}, {"ROLLBACK", 1, false}} {
		t.Run(tc.kind+money(tc.amount).Amount(), func(t *testing.T) {
			err := operation(tc.kind, tc.amount).Validate()
			if (err == nil) != tc.valid {
				t.Fatal(err)
			}
		})
	}
	o := operation("BET", 1)
	o.ReferenceExternalTransactionID = "reference"
	if o.Validate() == nil {
		t.Fatal("BET reference accepted")
	}
	o.Kind = "WIN"
	if err := o.Validate(); err != nil {
		t.Fatal(err)
	}
}
func TestTransactionTransitions(t *testing.T) {
	now := time.Now().UTC()
	o := operation("BET", 100)
	tx, err := NewTransaction("tx", "key", o, [32]byte{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if tx.Status() != "PENDING" {
		t.Fatal(tx.Status())
	}
	if err = tx.Complete(money(50), "", now); err != nil || tx.Status() != "PROCESSED" {
		t.Fatal(err)
	}
	before := *tx
	if err = tx.Complete(money(50), "INSUFFICIENT_FUNDS", now); !errors.Is(err, ErrInvalidTransition) || *tx != before {
		t.Fatal(err)
	}
	rejected, err := NewTransaction("tx2", "key2", o, [32]byte{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = rejected.Complete(money(50), "INSUFFICIENT_FUNDS", now); err != nil || rejected.Status() != "REJECTED" {
		t.Fatal(err)
	}
	if err = rejected.Complete(money(50), "", now); !errors.Is(err, ErrInvalidTransition) {
		t.Fatal(err)
	}
	pending, _ := NewTransaction("tx3", "key3", o, [32]byte{}, now)
	if err = pending.Complete(money(-1), "", now); !errors.Is(err, ErrInvalidTransaction) || pending.Status() != "PENDING" {
		t.Fatal(err)
	}
}

func TestPendingReferenceTransitions(t *testing.T) {
	now := time.Now().UTC()
	operation := operation("REFUND", 100)
	operation.ReferenceExternalTransactionID = "missing"
	tx, err := NewTransaction("id", "key", operation, [32]byte{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.AwaitReference(money(500), now); err != nil || tx.Status() != "PENDING_REFERENCE" {
		t.Fatal(err)
	}
	if err = tx.AwaitReference(money(500), now); !errors.Is(err, ErrInvalidTransition) {
		t.Fatal(err)
	}
	saved, err := RestorePending("id", "key", operation, [32]byte{}, money(500), now, now)
	if err != nil || saved.Status() != "PENDING_REFERENCE" {
		t.Fatal(saved, err)
	}
	if err = saved.Complete(money(600), "", now.Add(time.Second)); err != nil || saved.Status() != "PROCESSED" {
		t.Fatal(err)
	}
	if err = saved.Complete(money(500), "", now); !errors.Is(err, ErrInvalidTransition) {
		t.Fatal(err)
	}
}

func TestSelfReferenceRejected(t *testing.T) {
	o := operation("ROLLBACK", 100)
	o.ReferenceExternalTransactionID = o.ExternalTransactionID
	if err := o.Validate(); !errors.Is(err, ErrInvalidTransaction) {
		t.Fatal(err)
	}
}
