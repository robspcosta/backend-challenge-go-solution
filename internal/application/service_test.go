package application

import (
	"encoding/json"
	"errors"
	"example.com/wagering/internal/domain"
	"testing"
)

func TestCanonicalPayload(t *testing.T) {
	a, _ := domain.ParseMoney("00025.00", "BRL")
	b, _ := domain.ParseMoney("25.00", "BRL")
	op := domain.Operation{ProviderID: "p", ExternalTransactionID: "e", PlayerID: "player", WalletID: "wallet", RoundID: "r", GameID: "g", Kind: "BET", Money: a}
	payload, hash, err := CanonicalPayload(op)
	if err != nil {
		t.Fatal(err)
	}
	const expected = `{"externalTransactionId":"e","gameId":"g","kind":"BET","money":{"amount":"25.00","currency":"BRL"},"playerId":"player","providerId":"p","roundId":"r","walletId":"wallet"}`
	if string(payload) != expected {
		t.Fatalf("%s", payload)
	}
	op.Money = b
	_, same, err := CanonicalPayload(op)
	if err != nil || hash != same {
		t.Fatal("normalization differs")
	}
	op.GameID = "other"
	_, different, err := CanonicalPayload(op)
	if err != nil || hash == different {
		t.Fatal("changed payload has same hash")
	}
	var payloadFields map[string]any
	if err = json.Unmarshal(payload, &payloadFields); err != nil {
		t.Fatal(err)
	}
	if _, exists := payloadFields["idempotencyKey"]; exists {
		t.Fatal("key in hash")
	}
}
func TestReplayConflict(t *testing.T) {
	hash := [32]byte{1}
	balance, _ := domain.FromCents(7500, "BRL")
	existing := []Stored{{View: TransactionView{Result: Result{TransactionID: "t", Status: "PROCESSED", Balance: balance}}, Hash: hash[:]}}
	result, err := replay(existing, hash)
	if err != nil || !result.IdempotentReplay || result.Balance.Cents() != 7500 {
		t.Fatal(result, err)
	}
	if _, err = replay(existing, [32]byte{2}); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if _, err = replay(append(existing, existing[0]), hash); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
}
func TestUUID(t *testing.T) {
	id, err := UUID("0192F28F-5DC0-7D58-BDB2-814AD6A0F4A1")
	if err != nil || id != "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1" {
		t.Fatal(id, err)
	}
	for _, id := range []string{"", "garbage", "00000000-0000-0000-0000-000000000000", "0192f28f5dc07d58bdb2814ad6a0f4a1"} {
		if _, err := UUID(id); err == nil {
			t.Fatal(id)
		}
	}
	generated, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = UUID(generated); err != nil {
		t.Fatal(err)
	}
}
