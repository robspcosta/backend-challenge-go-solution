//go:build integration

package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"example.com/wagering/internal/application"
	"example.com/wagering/internal/domain"
	"example.com/wagering/internal/platform/repository"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"
)

func setupReferences(t *testing.T) *fixture {
	t.Helper()
	return setupFinancial(t)
}
func mustProcess(t *testing.T, f *fixture, key string, o domain.Operation, status string) application.Result {
	t.Helper()
	result, err := f.services[0].Process(context.Background(), key, o)
	if err != nil || result.Status != status {
		t.Fatalf("process %s: %+v %v", key, result, err)
	}
	return result
}
func TestReversalsAndCombinationPolicy(t *testing.T) {
	f := setupReferences(t)
	w := newWallet(t, f, "100.00")
	bet := op(t, w, "bet-30", "BET", "30.00")
	mustProcess(t, f, "bet-30", bet, "PROCESSED")
	refund := op(t, w, "refund-30", "REFUND", "30.00")
	refund.ReferenceExternalTransactionID = bet.ExternalTransactionID
	first := mustProcess(t, f, "refund-30", refund, "PROCESSED")
	if first.Balance.Cents() != 10000 {
		t.Fatal(first)
	}
	second := refund
	second.ExternalTransactionID = "refund-again"
	duplicate := mustProcess(t, f, "refund-again", second, "REJECTED")
	if duplicate.FailureCode != "ALREADY_REVERSED" {
		t.Fatal(duplicate)
	}
	direct := op(t, w, "rollback-bet", "ROLLBACK", "30.00")
	direct.ReferenceExternalTransactionID = bet.ExternalTransactionID
	blocked := mustProcess(t, f, "rollback-bet", direct, "REJECTED")
	if blocked.FailureCode != "ALREADY_REVERSED" {
		t.Fatal(blocked)
	}
	reverseRefund := op(t, w, "rollback-refund", "ROLLBACK", "30.00")
	reverseRefund.ReferenceExternalTransactionID = refund.ExternalTransactionID
	rb := mustProcess(t, f, "rollback-refund", reverseRefund, "PROCESSED")
	if rb.Balance.Cents() != 7000 {
		t.Fatal(rb)
	}
	repeat := reverseRefund
	repeat.ExternalTransactionID = "rollback-refund-two"
	denied := mustProcess(t, f, "rollback-refund-two", repeat, "REJECTED")
	if denied.FailureCode != "ALREADY_REVERSED" {
		t.Fatal(denied)
	}
	invalid := op(t, w, "partial-refund", "REFUND", "10.00")
	invalid.ReferenceExternalTransactionID = bet.ExternalTransactionID
	partial := mustProcess(t, f, "partial-refund", invalid, "REJECTED")
	if partial.FailureCode != "INVALID_REFERENCE" {
		t.Fatal(partial)
	}
	assertWallet(t, f, w, 7000, 4, 2)
}
func TestRollbackWinAndInsufficientFunds(t *testing.T) {
	f := setupReferences(t)
	w := newWallet(t, f, "100.00")
	win := op(t, w, "win-1", "WIN", "50.00")
	mustProcess(t, f, "win-1", win, "PROCESSED")
	rollback := op(t, w, "rollback-win", "ROLLBACK", "50.00")
	rollback.ReferenceExternalTransactionID = win.ExternalTransactionID
	got := mustProcess(t, f, "rollback-win", rollback, "PROCESSED")
	if got.Balance.Cents() != 10000 {
		t.Fatal(got)
	}
	secondWin := op(t, w, "win-2", "WIN", "100.00")
	mustProcess(t, f, "win-2", secondWin, "PROCESSED")
	bet := op(t, w, "spend", "BET", "150.00")
	mustProcess(t, f, "spend", bet, "PROCESSED")
	impossible := op(t, w, "rollback-win-2", "ROLLBACK", "100.00")
	impossible.ReferenceExternalTransactionID = secondWin.ExternalTransactionID
	denied := mustProcess(t, f, "rollback-win-2", impossible, "REJECTED")
	if denied.FailureCode != "REVERSAL_INSUFFICIENT_FUNDS" || denied.Balance.Cents() != 5000 {
		t.Fatal(denied)
	}
	assertWallet(t, f, w, 5000, 5, 2)
}
func TestReferenceArrivesAfterRefundAndRestart(t *testing.T) {
	f := setupReferences(t)
	w := newWallet(t, f, "100.00")
	refund := op(t, w, "early-refund", "REFUND", "30.00")
	refund.ReferenceExternalTransactionID = "late-bet"
	code, data, err := send(f.servers[0].URL, "POST", "/wagering/transactions", "early-refund", refund)
	if err != nil || code != 202 {
		t.Fatal(code, string(data), err)
	}
	var pending application.Result
	if err = json.Unmarshal(data, &pending); err != nil || pending.Status != "PENDING_REFERENCE" {
		t.Fatal(pending, err)
	}
	code, data, err = send(f.servers[1].URL, "POST", "/wagering/transactions", "early-refund", refund)
	var replay application.Result
	if err == nil {
		err = json.Unmarshal(data, &replay)
	}
	if err != nil || code != 202 || !replay.IdempotentReplay || replay.TransactionID != pending.TransactionID {
		t.Fatal(code, replay, err)
	}
	assertWallet(t, f, w, 10000, 1, 0)
	bet := op(t, w, "late-bet", "BET", "30.00")
	mustProcess(t, f, "late-bet", bet, "PROCESSED")
	if _, err = f.conn.Exec(context.Background(), "UPDATE wager_transaction SET reference_next_attempt_at=now()-interval '1 second' WHERE id=$1", pending.TransactionID); err != nil {
		t.Fatal(err)
	}
	// A fresh pool has no in-memory knowledge of the accepted operation.
	var schema string
	if err = f.conn.QueryRow(context.Background(), "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	restarted := application.New(repository.New(pool))
	if err = restarted.ResolveDue(context.Background(), pending.TransactionID); err != nil {
		t.Fatal(err)
	}
	completed, err := restarted.Transaction(context.Background(), pending.TransactionID)
	if err != nil || completed.Status != "PROCESSED" || completed.Balance.Cents() != 10000 {
		t.Fatal(completed, err)
	}
	assertWallet(t, f, w, 10000, 3, 1)
	replay, err = restarted.Process(context.Background(), "early-refund", refund)
	if err != nil || !replay.IdempotentReplay || replay.Balance.Cents() != 10000 {
		t.Fatal(replay, err)
	}
}
func TestReferenceExpiry(t *testing.T) {
	f := setupReferences(t)
	w := newWallet(t, f, "100.00")
	rollback := op(t, w, "orphan", "ROLLBACK", "20.00")
	rollback.ReferenceExternalTransactionID = "never-arrived"
	pending := mustProcess(t, f, "orphan", rollback, "PENDING_REFERENCE")
	if _, err := f.conn.Exec(context.Background(), "UPDATE wager_transaction SET reference_attempts=7,reference_next_attempt_at=now()-interval '1 second',reference_expires_at=now()-interval '1 second' WHERE id=$1", pending.TransactionID); err != nil {
		t.Fatal(err)
	}
	if err := f.services[2].ResolveDue(context.Background(), pending.TransactionID); err != nil {
		t.Fatal(err)
	}
	result, err := f.services[2].Transaction(context.Background(), pending.TransactionID)
	if err != nil || result.Status != "REJECTED" || result.FailureCode != "REFERENCE_NOT_FOUND" {
		t.Fatal(result, err)
	}
	assertWallet(t, f, w, 10000, 1, 0)
	if _, err = f.services[2].Process(context.Background(), "orphan", rollback); err != nil {
		t.Fatal(err)
	}
}
func TestConcurrentCompensations(t *testing.T) {
	f := setupReferences(t)
	w := newWallet(t, f, "100.00")
	bet := op(t, w, "bet-50", "BET", "50.00")
	mustProcess(t, f, "bet-50", bet, "PROCESSED")
	refund := op(t, w, "refund-50", "REFUND", "50.00")
	refund.ReferenceExternalTransactionID = bet.ExternalTransactionID
	rollback := op(t, w, "rollback-50", "ROLLBACK", "50.00")
	rollback.ReferenceExternalTransactionID = bet.ExternalTransactionID
	results := parallel(f, []domain.Operation{refund, rollback}, []string{"refund-key", "rollback-key"})
	success, rejected := 0, 0
	for _, out := range results {
		if out.err != nil {
			t.Fatal(out.err)
		}
		switch out.result.Status {
		case "PROCESSED":
			success++
			if out.code != 200 {
				t.Fatal(out)
			}
		case "REJECTED":
			rejected++
			if out.code != 422 || out.result.FailureCode != "ALREADY_REVERSED" {
				t.Fatal(out)
			}
		default:
			t.Fatal(out)
		}
	}
	if success != 1 || rejected != 1 {
		t.Fatal(success, rejected)
	}
	assertWallet(t, f, w, 10000, 3, 1)
}
func TestWorkerLifecycleResolvesPending(t *testing.T) {
	f := setupReferences(t)
	w := newWallet(t, f, "100.00")
	pendingOp := op(t, w, "worker-refund", "REFUND", "25.00")
	pendingOp.ReferenceExternalTransactionID = "worker-bet"
	pending := mustProcess(t, f, "worker-refund", pendingOp, "PENDING_REFERENCE")
	bet := op(t, w, "worker-bet", "BET", "25.00")
	mustProcess(t, f, "worker-bet", bet, "PROCESSED")
	var schema string
	if err := f.conn.QueryRow(context.Background(), "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	dsn := os.Getenv("TEST_DATABASE_URL") + "&search_path=" + schema
	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("HTTP_ADDR", "127.0.0.1:0")
	applicationApp := fx.New(Options(), fx.NopLogger)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := applicationApp.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := applicationApp.Stop(ctx); err != nil {
			t.Error(err)
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		stored, err := f.services[0].Transaction(context.Background(), pending.TransactionID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Status == "PROCESSED" {
			assertWallet(t, f, w, 10000, 3, 1)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("worker did not resolve pending reference")
}

func TestTwoWorkersOnePendingReference(t *testing.T) {
	f := setupReferences(t)
	w := newWallet(t, f, "100.00")
	refund := op(t, w, "two-workers-refund", "REFUND", "20.00")
	refund.ReferenceExternalTransactionID = "two-workers-bet"
	pending := mustProcess(t, f, "two-workers-refund", refund, "PENDING_REFERENCE")
	bet := op(t, w, "two-workers-bet", "BET", "20.00")
	mustProcess(t, f, "two-workers-bet", bet, "PROCESSED")
	if _, err := f.conn.Exec(context.Background(), "UPDATE wager_transaction SET reference_next_attempt_at=now()-interval '1 second' WHERE id=$1", pending.TransactionID); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	outcomes := make([]error, 2)
	for i := range outcomes {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			outcomes[i] = f.services[i].ResolveDue(context.Background(), pending.TransactionID)
		}(i)
	}
	group.Wait()
	for _, err := range outcomes {
		if err != nil {
			t.Fatal(err)
		}
	}
	result, err := f.services[2].Transaction(context.Background(), pending.TransactionID)
	if err != nil || result.Status != "PROCESSED" {
		t.Fatal(result, err)
	}
	assertWallet(t, f, w, 10000, 3, 1)
	var count int
	if err = f.conn.QueryRow(context.Background(), "SELECT count(*) FROM wallet_ledger_entry WHERE transaction_id=$1", pending.TransactionID).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
}
func TestPendingReferenceChain(t *testing.T) {
	f := setupReferences(t)
	w := newWallet(t, f, "100.00")
	refund := op(t, w, "chain-refund", "REFUND", "30.00")
	refund.ReferenceExternalTransactionID = "chain-bet"
	refundResult := mustProcess(t, f, "chain-refund", refund, "PENDING_REFERENCE")
	rollback := op(t, w, "chain-rollback", "ROLLBACK", "30.00")
	rollback.ReferenceExternalTransactionID = refund.ExternalTransactionID
	rollbackResult := mustProcess(t, f, "chain-rollback", rollback, "PENDING_REFERENCE")
	bet := op(t, w, "chain-bet", "BET", "30.00")
	mustProcess(t, f, "chain-bet", bet, "PROCESSED")
	if _, err := f.conn.Exec(context.Background(), "UPDATE wager_transaction SET reference_next_attempt_at=now()-interval '1 second' WHERE id IN ($1,$2)", refundResult.TransactionID, rollbackResult.TransactionID); err != nil {
		t.Fatal(err)
	}
	if err := f.services[0].ResolveDue(context.Background(), refundResult.TransactionID); err != nil {
		t.Fatal(err)
	}
	if err := f.services[1].ResolveDue(context.Background(), rollbackResult.TransactionID); err != nil {
		t.Fatal(err)
	}
	result, err := f.services[2].Transaction(context.Background(), rollbackResult.TransactionID)
	if err != nil || result.Status != "PROCESSED" || result.Balance.Cents() != 7000 {
		t.Fatal(result, err)
	}
	assertWallet(t, f, w, 7000, 4, 2)
}
func TestReferenceMigrationReversal(t *testing.T) {
	f := setupReferences(t)
	down, err := os.ReadFile(filepath.Join("..", "..", "migrations", "000002_references.down.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.conn.Exec(context.Background(), string(down)); err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err = f.conn.QueryRow(context.Background(), "SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='wager_transaction' AND column_name='reference_attempts')").Scan(&exists); err != nil || exists {
		t.Fatal("down migration retained scheduling column", err)
	}
	up, err := os.ReadFile(filepath.Join("..", "..", "migrations", "000002_references.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.conn.Exec(context.Background(), string(up)); err != nil {
		t.Fatal(err)
	}
	if err = f.conn.QueryRow(context.Background(), "SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='wager_transaction' AND column_name='reference_attempts')").Scan(&exists); err != nil || !exists {
		t.Fatal("up migration missing scheduling column", err)
	}
}
