//go:build integration

package app

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestReconciliationSnapshotAndDivergence(t *testing.T) {
	f := setupFinancial(t)
	w := newWallet(t, f, "100.00")
	operation := op(t, w, "reconciliation-bet", "BET", "25.00")
	if result, err := f.services[0].Process(context.Background(), "reconciliation-key", operation); err != nil || result.Status != "PROCESSED" {
		t.Fatalf("bet: %+v %v", result, err)
	}
	code, body, err := send(f.servers[0].URL, http.MethodPost, "/wallets/"+w.ID+"/reconciliation", "", nil)
	if err != nil || code != 200 {
		t.Fatalf("reconcile: %d %s %v", code, body, err)
	}
	var got struct {
		Consistent     bool  `json:"consistent"`
		CheckedEntries int64 `json:"checkedEntries"`
		StoredBalance  struct {
			Amount string `json:"amount"`
		} `json:"storedBalance"`
		CalculatedBalance struct {
			Amount string `json:"amount"`
		} `json:"calculatedBalance"`
		Difference struct {
			Amount string `json:"amount"`
		} `json:"difference"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if !got.Consistent || got.StoredBalance.Amount != "75.00" || got.CalculatedBalance.Amount != "75.00" || got.Difference.Amount != "0.00" || got.CheckedEntries != 2 {
		t.Fatalf("unexpected reconciliation: %+v", got)
	}
	// Simulate corruption only in this disposable schema; production triggers remain enabled.
	if _, err := f.conn.Exec(context.Background(), `ALTER TABLE wallet DISABLE TRIGGER wallet_financial_snapshot`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.conn.Exec(context.Background(), `UPDATE wallet SET balance_cents=7400 WHERE id=$1`, w.ID); err != nil {
		t.Fatal(err)
	}
	code, body, err = send(f.servers[0].URL, http.MethodPost, "/wallets/"+w.ID+"/reconciliation", "", nil)
	if err != nil || code != 200 {
		t.Fatalf("divergence: %d %s %v", code, body, err)
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Consistent || got.Difference.Amount != "-1.00" || got.StoredBalance.Amount != "74.00" || got.CalculatedBalance.Amount != "75.00" {
		t.Fatalf("missing divergence: %+v", got)
	}
	var stored int64
	if err := f.conn.QueryRow(context.Background(), `SELECT balance_cents FROM wallet WHERE id=$1`, w.ID).Scan(&stored); err != nil || stored != 7400 {
		t.Fatalf("reconciliation mutated wallet: %d %v", stored, err)
	}
}
