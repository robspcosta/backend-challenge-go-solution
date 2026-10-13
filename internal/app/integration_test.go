//go:build integration

package app

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"
)

// Requires real PostgreSQL; each run uses its own schema and leaves no financial data.
func TestPostgresSchemaAndLifecycle(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("TEST_DATABASE_URL must point to a real PostgreSQL container")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	schema := fmt.Sprintf("itest_%d", time.Now().UnixNano())
	if _, err = conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := conn.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
	}()
	if _, err = conn.Exec(ctx, "SET search_path TO "+schema); err != nil {
		t.Fatal(err)
	}
	sql, err := os.ReadFile(filepath.Join("..", "..", "migrations", "000001_financial_schema.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, string(sql)); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	const wallet = "00000000-0000-0000-0000-000000000001"
	const player = "00000000-0000-0000-0000-000000000002"
	const opening = "00000000-0000-0000-0000-000000000003"
	tx, err = conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, "INSERT INTO wallet(id,player_id,currency,balance_cents) VALUES ($1,$2,'BRL',10000)", wallet, player); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, "INSERT INTO wager_transaction(id,wallet_id,player_id,currency,kind,amount_cents,status,result_balance_cents) VALUES ($1,$2,$3,'BRL','OPENING',10000,'PROCESSED',10000)", opening, wallet, player); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, "INSERT INTO wallet_ledger_entry(id,wallet_id,transaction_id,currency,direction,amount_cents,balance_before_cents,balance_after_cents) VALUES ('00000000-0000-0000-0000-000000000004',$1,$2,'BRL','CREDIT',10000,0,10000)", wallet, opening); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"UPDATE wallet SET balance_cents=-1",
		"UPDATE wallet SET balance_cents=9999",
		"UPDATE wallet_ledger_entry SET amount_cents=9999",
		"DELETE FROM wallet_ledger_entry",
		"TRUNCATE wallet_ledger_entry",
		"UPDATE wager_transaction SET result_balance_cents=9999",
		"INSERT INTO wallet(id,player_id,currency,balance_cents) SELECT '00000000-0000-0000-0000-000000000005',player_id,currency,0 FROM wallet",
		"INSERT INTO wallet_ledger_entry SELECT '00000000-0000-0000-0000-000000000006',wallet_id,transaction_id,currency,direction,amount_cents,balance_before_cents,balance_after_cents,created_at FROM wallet_ledger_entry",
	} {
		if _, err = conn.Exec(ctx, statement); err == nil {
			t.Fatalf("constraint allowed %s", statement)
		}
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	t.Setenv("DATABASE_URL", parsed.String())
	t.Setenv("HTTP_ADDR", "127.0.0.1:0")
	var pool *pgxpool.Pool
	application := fx.New(Options(), fx.Populate(&pool), fx.NopLogger)
	if err = application.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err = pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if err = application.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err = pool.Ping(ctx); err == nil {
		t.Fatal("pool still usable after shutdown")
	}
	down, err := os.ReadFile(filepath.Join("..", "..", "migrations", "000001_financial_schema.down.sql"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err = conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, string(down)); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err = conn.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", schema+".wallet").Scan(&exists); err != nil || exists {
		t.Fatal("down migration failed", err)
	}
	// Reapply verifies that up/down leave no orphan functions.
	if _, err = conn.Exec(ctx, strings.TrimSpace(string(sql))); err != nil {
		t.Fatal(err)
	}
}
