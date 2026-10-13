//go:build integration

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"example.com/wagering/internal/application"
	"example.com/wagering/internal/domain"
	"example.com/wagering/internal/platform/httpapi"
	"example.com/wagering/internal/platform/repository"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type fixture struct {
	conn     *pgx.Conn
	pools    []*pgxpool.Pool
	services []*application.Service
	servers  []*httptest.Server
}

func setupFinancial(t *testing.T) *fixture {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("TEST_DATABASE_URL must point to a real Postgres container")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("itest_%d", time.Now().UnixNano())
	if _, err = conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		_ = conn.Close(ctx)
		t.Fatal(err)
	}
	f := &fixture{conn: conn}
	t.Cleanup(func() {
		for _, server := range f.servers {
			server.Close()
		}
		for _, pool := range f.pools {
			pool.Close()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := conn.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
		_ = conn.Close(ctx)
	})
	if _, err = conn.Exec(ctx, "SET search_path TO "+schema); err != nil {
		t.Fatal(err)
	}
	sql, err := os.ReadFile(filepath.Join("..", "..", "migrations", "000001_financial_schema.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(ctx, string(sql)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"000002_references.up.sql", "000003_messaging.up.sql"} {
		migration, readErr := os.ReadFile(filepath.Join("..", "..", "migrations", name))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if _, err = conn.Exec(ctx, string(migration)); err != nil {
			t.Fatal(err)
		}
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	for i := 0; i < 3; i++ {
		cfg, err := pgxpool.ParseConfig(parsed.String())
		if err != nil {
			t.Fatal(err)
		}
		cfg.MaxConns = 20
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		f.pools = append(f.pools, pool)
		service := application.New(repository.New(pool))
		f.services = append(f.services, service)
		mux := http.NewServeMux()
		httpapi.New(service).Register(mux)
		f.servers = append(f.servers, httptest.NewServer(mux))
	}
	return f
}
func m(t *testing.T, amount string) domain.Money {
	t.Helper()
	value, err := domain.ParseMoney(amount, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return value
}
func newWallet(t *testing.T, f *fixture, amount string) application.WalletView {
	t.Helper()
	id, err := application.NewID()
	if err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{"playerId": id, "initialBalance": m(t, amount)}
	code, data, err := send(f.servers[0].URL, "POST", "/wallets", "", payload)
	if err != nil || code != 201 {
		t.Fatalf("create: %d %s %v", code, data, err)
	}
	var w application.WalletView
	if err = json.Unmarshal(data, &w); err != nil {
		t.Fatal(err)
	}
	return w
}
func op(t *testing.T, w application.WalletView, external, kind, amount string) domain.Operation {
	return domain.Operation{ProviderID: "provider-a", ExternalTransactionID: external, PlayerID: w.PlayerID, WalletID: w.ID, RoundID: "round-1", GameID: "game-1", Kind: kind, Money: m(t, amount)}
}
func send(base, method, path, key string, body any) (int, []byte, error) {
	var encoded []byte
	var err error
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
	}
	request, err := http.NewRequest(method, base+path, bytes.NewReader(encoded))
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	var buf bytes.Buffer
	_, err = buf.ReadFrom(response.Body)
	return response.StatusCode, buf.Bytes(), err
}

type outcome struct {
	code   int
	result application.Result
	err    error
}

func parallel(f *fixture, operations []domain.Operation, keys []string) []outcome {
	outcomes := make([]outcome, len(operations))
	start := make(chan struct{})
	var wait sync.WaitGroup
	for i := range operations {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			<-start
			code, data, err := send(f.servers[i%3].URL, "POST", "/wagering/transactions", keys[i], operations[i])
			outcomes[i].code = code
			outcomes[i].err = err
			if err == nil {
				outcomes[i].err = json.Unmarshal(data, &outcomes[i].result)
			}
		}(i)
	}
	close(start)
	wait.Wait()
	return outcomes
}
func assertWallet(t *testing.T, f *fixture, w application.WalletView, cents, version, debits int64) {
	t.Helper()
	ctx := context.Background()
	actual, err := f.services[1].Wallet(ctx, w.ID)
	if err != nil || actual.Balance.Cents() != cents || actual.Version != version {
		t.Fatalf("wallet=%+v err=%v", actual, err)
	}
	var count int64
	if err = f.conn.QueryRow(ctx, "SELECT COUNT(*) FROM wallet_ledger_entry WHERE wallet_id=$1 AND direction='DEBIT'", w.ID).Scan(&count); err != nil || count != debits {
		t.Fatalf("debits=%d err=%v", count, err)
	}
	var reconstructed int64
	if err = f.conn.QueryRow(ctx, "SELECT COALESCE(SUM(CASE direction WHEN 'CREDIT' THEN amount_cents ELSE -amount_cents END),0)::bigint FROM wallet_ledger_entry WHERE wallet_id=$1", w.ID).Scan(&reconstructed); err != nil || reconstructed != cents {
		t.Fatalf("ledger=%d err=%v", reconstructed, err)
	}
}
func TestSameBet50Times(t *testing.T) {
	f := setupFinancial(t)
	w := newWallet(t, f, "100.00")
	operation := op(t, w, "same-bet", "BET", "25.00")
	operations := make([]domain.Operation, 50)
	keys := make([]string, 50)
	for i := range operations {
		operations[i] = operation
		keys[i] = "stable-key"
	}
	results := parallel(f, operations, keys)
	firstID := ""
	fresh := 0
	for _, out := range results {
		if out.err != nil || out.code != 200 || out.result.Status != "PROCESSED" || out.result.Balance.Cents() != 7500 {
			t.Fatalf("outcome=%+v", out)
		}
		if firstID == "" {
			firstID = out.result.TransactionID
		}
		if out.result.TransactionID != firstID {
			t.Fatal("different IDs for duplicate")
		}
		if !out.result.IdempotentReplay {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatalf("fresh=%d", fresh)
	}
	assertWallet(t, f, w, 7500, 2, 1)
	var count int
	if err := f.conn.QueryRow(context.Background(), "SELECT COUNT(*) FROM wager_transaction WHERE wallet_id=$1 AND kind='BET'", w.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	// A later WIN must not change the balance returned by the earlier BET replay.
	win := op(t, w, "win-1", "WIN", "10.00")
	code, data, err := send(f.servers[1].URL, "POST", "/wagering/transactions", "win-key", win)
	if err != nil || code != 200 {
		t.Fatal(code, string(data), err)
	}
	code, data, err = send(f.servers[2].URL, "POST", "/wagering/transactions", "another-key", operation)
	var replay application.Result
	if err == nil {
		err = json.Unmarshal(data, &replay)
	}
	if err != nil || code != 200 || !replay.IdempotentReplay || replay.Balance.Cents() != 7500 {
		t.Fatal(code, string(data), err)
	}
	assertWallet(t, f, w, 8500, 3, 1)
	changed := operation
	changed.Money = m(t, "26.00")
	code, data, err = send(f.servers[0].URL, "POST", "/wagering/transactions", "stable-key", changed)
	if err != nil || code != 409 {
		t.Fatal(code, string(data), err)
	}
	assertWallet(t, f, w, 8500, 3, 1)
	// A new pool/service after closing the original pools proves durable replay.
	for _, pool := range f.pools {
		pool.Close()
	}
	cfg, err := pgxpool.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	var schema string
	if err = f.conn.QueryRow(context.Background(), "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	restarted := application.New(repository.New(pool))
	replay, err = restarted.Process(context.Background(), "stable-key", operation)
	if err != nil || !replay.IdempotentReplay || replay.Balance.Cents() != 7500 {
		t.Fatal(replay, err)
	}
}
func TestTwoBets80On100(t *testing.T) {
	f := setupFinancial(t)
	w := newWallet(t, f, "100.00")
	operations := []domain.Operation{op(t, w, "bet-a", "BET", "80.00"), op(t, w, "bet-b", "BET", "80.00")}
	keys := []string{"key-a", "key-b"}
	results := parallel(f, operations, keys)
	processed, rejected := 0, 0
	for _, out := range results {
		if out.err != nil {
			t.Fatal(out.err)
		}
		switch out.result.Status {
		case "PROCESSED":
			processed++
			if out.code != 200 {
				t.Fatal(out)
			}
		case "REJECTED":
			rejected++
			if out.code != 422 || out.result.FailureCode != "INSUFFICIENT_FUNDS" {
				t.Fatal(out)
			}
		default:
			t.Fatal(out)
		}
		if out.result.Balance.Cents() != 2000 {
			t.Fatal(out)
		}
	}
	if processed != 1 || rejected != 1 {
		t.Fatal(processed, rejected)
	}
	assertWallet(t, f, w, 2000, 2, 1)
	replays := parallel(f, operations, keys)
	for i, out := range replays {
		if out.err != nil || !out.result.IdempotentReplay || out.result.TransactionID != results[i].result.TransactionID || out.result.Status != results[i].result.Status {
			t.Fatal(out)
		}
	}
	assertWallet(t, f, w, 2000, 2, 1)
}
func TestLossReadsAndOpening(t *testing.T) {
	f := setupFinancial(t)
	w := newWallet(t, f, "0.00")
	loss := op(t, w, "loss", "LOSS", "0.00")
	code, data, err := send(f.servers[0].URL, "POST", "/wagering/transactions", "loss-key", loss)
	if err != nil || code != 200 {
		t.Fatal(code, string(data), err)
	}
	var result application.Result
	if err = json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	assertWallet(t, f, w, 0, 1, 0)
	for _, path := range []string{"/wallets/" + w.ID, "/wallets/" + w.ID + "/ledger", "/wagering/transactions/" + result.TransactionID, "/providers/provider-a/wagering/transactions/loss"} {
		code, data, err = send(f.servers[1].URL, "GET", path, "", nil)
		if err != nil || code != 200 {
			t.Fatal(path, code, string(data), err)
		}
	}
	code, data, err = send(f.servers[0].URL, "POST", "/wallets", "", map[string]any{"playerId": w.PlayerID, "initialBalance": m(t, "0.00")})
	if err != nil || code != 409 {
		t.Fatal(code, string(data), err)
	}
	var count int
	if err = f.conn.QueryRow(context.Background(), "SELECT COUNT(*) FROM wager_transaction WHERE wallet_id=$1 AND kind='OPENING'", w.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	positive := newWallet(t, f, "10.00")
	win := op(t, positive, "win", "WIN", "1.00")
	if _, err = f.services[0].Process(context.Background(), "win", win); err != nil {
		t.Fatal(err)
	}
	page, err := f.services[0].Ledger(context.Background(), positive.ID, "", 1)
	if err != nil || len(page.Entries) != 1 || page.NextCursor == "" {
		t.Fatal(page, err)
	}
	next, err := f.services[0].Ledger(context.Background(), positive.ID, page.NextCursor, 1)
	if err != nil || len(next.Entries) != 1 || next.NextCursor != "" || next.Entries[0].ID == page.Entries[0].ID {
		t.Fatal(next, err)
	}
}
func TestIndependentWalletDoesNotWaitForLockedWallet(t *testing.T) {
	f := setupFinancial(t)
	locked := newWallet(t, f, "100.00")
	other := newWallet(t, f, "100.00")
	tx, err := f.conn.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(context.Background(), "SELECT id FROM wallet WHERE id=$1 FOR UPDATE", locked.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err = f.services[1].Process(ctx, "independent", op(t, other, "other-bet", "BET", "10.00")); err != nil {
		t.Fatal("independent wallet blocked", err)
	}
}

func TestIdentityCollisionAcrossWallets(t *testing.T) {
	f := setupFinancial(t)
	a := newWallet(t, f, "100.00")
	b := newWallet(t, f, "100.00")
	operations := []domain.Operation{op(t, a, "a", "BET", "10.00"), op(t, b, "b", "BET", "10.00")}
	results := parallel(f, operations, []string{"shared-key", "shared-key"})
	success, conflicts := 0, 0
	for i, out := range results {
		if out.err != nil {
			t.Fatal(out.err)
		}
		w := a
		if i == 1 {
			w = b
		}
		switch out.code {
		case 200:
			success++
			assertWallet(t, f, w, 9000, 2, 1)
		case 409:
			conflicts++
			assertWallet(t, f, w, 10000, 1, 0)
		default:
			t.Fatal(out)
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatal(success, conflicts)
	}
}
func TestWinLimitsAndReferences(t *testing.T) {
	f := setupFinancial(t)
	maximum := newWallet(t, f, "92233720368547758.07")
	result, err := f.services[0].Process(context.Background(), "overflow", op(t, maximum, "overflow", "WIN", "0.01"))
	if err != nil || result.Status != "REJECTED" || result.FailureCode != "NUMERIC_LIMIT" {
		t.Fatal(result, err)
	}
	assertWallet(t, f, maximum, 9223372036854775807, 1, 0)
	w := newWallet(t, f, "100.00")
	bet := op(t, w, "reference-bet", "BET", "10.00")
	if _, err = f.services[0].Process(context.Background(), "reference-bet", bet); err != nil {
		t.Fatal(err)
	}
	win := op(t, w, "referenced-win", "WIN", "20.00")
	win.ReferenceExternalTransactionID = bet.ExternalTransactionID
	result, err = f.services[1].Process(context.Background(), "referenced-win", win)
	if err != nil || result.Status != "PROCESSED" {
		t.Fatal(result, err)
	}
	assertWallet(t, f, w, 11000, 3, 1)
	invalid := win
	invalid.ExternalTransactionID = "invalid-reference"
	invalid.RoundID = "other-round"
	result, err = f.services[2].Process(context.Background(), "invalid-reference", invalid)
	if err != nil || result.Status != "REJECTED" || result.FailureCode != "INVALID_REFERENCE" {
		t.Fatal(result, err)
	}
	assertWallet(t, f, w, 11000, 3, 1)
}
