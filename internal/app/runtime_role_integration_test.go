//go:build integration

package app

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"example.com/wagering/internal/application"
	"example.com/wagering/internal/platform/repository"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRuntimeRoleMigrationAndPrivileges(t *testing.T) {
	f := setupFinancial(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	id, err := application.NewID()
	if err != nil {
		t.Fatal(err)
	}
	role := "wagering_runtime_" + id[:8]
	up, err := os.ReadFile("../../migrations/000004_runtime_role.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	down, err := os.ReadFile("../../migrations/000004_runtime_role.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	upSQL := strings.ReplaceAll(string(up), "wagering_runtime", role)
	downSQL := strings.ReplaceAll(string(down), "wagering_runtime", role)
	if _, err := f.conn.Exec(ctx, upSQL); err != nil {
		t.Fatal(err)
	}
	var runtimePool *pgxpool.Pool
	t.Cleanup(func() {
		if runtimePool != nil {
			runtimePool.Close()
		}
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if _, err := f.conn.Exec(cleanup, downSQL); err != nil {
			t.Error(err)
		}
	})
	cfg := f.pools[0].Config()
	cfg.ConnConfig.User = role
	cfg.ConnConfig.Password = "local-only-runtime"
	runtimePool, err = pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = runtimePool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	service := application.New(repository.New(runtimePool))
	playerID, err := application.NewID()
	if err != nil {
		t.Fatal(err)
	}
	wallet, err := service.CreateWallet(ctx, playerID, m(t, "100.00"))
	if err != nil {
		t.Fatal(err)
	}
	bet := op(t, wallet, "restricted-bet", "BET", "25.00")
	result, err := service.Process(ctx, "restricted-key", bet)
	if err != nil || result.Status != "PROCESSED" || result.Balance.Cents() != 7500 {
		t.Fatalf("runtime processing: %+v %v", result, err)
	}
	check, err := service.Reconcile(ctx, wallet.ID)
	if err != nil || !check.Consistent || check.CheckedEntries != 2 {
		t.Fatalf("runtime reconciliation: %+v %v", check, err)
	}
	events, err := repository.New(runtimePool).ClaimEvents(ctx, wallet.ID, 10)
	if err != nil || len(events) == 0 {
		t.Fatalf("runtime outbox claim: %d %v", len(events), err)
	}
	if err := repository.New(runtimePool).MarkPublished(ctx, events[0].EventID, wallet.ID); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`UPDATE wallet_ledger_entry SET amount_cents=1 WHERE wallet_id='` + wallet.ID + `'`,
		`DELETE FROM wallet WHERE id='` + wallet.ID + `'`,
		`TRUNCATE wallet_ledger_entry`,
		`CREATE TABLE unauthorized_runtime_table(id bigint)`,
	} {
		_, err := runtimePool.Exec(ctx, statement)
		var pgerr *pgconn.PgError
		if !errors.As(err, &pgerr) || pgerr.Code != "42501" {
			t.Fatalf("statement should fail with insufficient privilege: %s: %v", statement, err)
		}
	}
	// The role cannot remove financial protections or change audit history.
	if _, err := runtimePool.Exec(ctx, `ALTER TABLE wallet DISABLE TRIGGER wallet_financial_snapshot`); err == nil {
		t.Fatal("runtime role disabled financial trigger")
	}
}
