//go:build integration && load

package app

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"testing"
	"time"

	"example.com/wagering/internal/loadtest"
	"go.uber.org/fx"
)

func TestLoadHarnessRealContainers(t *testing.T) {
	if os.Getenv("TEST_OIDC_TOKEN_URL") == "" || os.Getenv("TEST_SQS_ENDPOINT") == "" {
		t.Skip("requires real Keycloak and LocalStack")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	_, _, _ = recoverySQS(t, ctx)
	f := setupFinancial(t)
	address := freeAddress(t)
	t.Setenv("DATABASE_URL", f.pools[0].Config().ConnConfig.ConnString())
	t.Setenv("HTTP_ADDR", address)
	t.Setenv("SQS_ENDPOINT", os.Getenv("TEST_SQS_ENDPOINT"))
	t.Setenv("OIDC_ISSUER", os.Getenv("TEST_OIDC_ISSUER"))
	t.Setenv("OIDC_JWKS_URL", os.Getenv("TEST_OIDC_JWKS_URL"))
	t.Setenv("OIDC_AUDIENCE", "wagering-api")
	t.Setenv("OIDC_INTERNAL_CLIENT", "wallet-internal")
	t.Setenv("OIDC_PROVIDER_CLIENTS", "provider-a,provider-b")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	app := fx.New(Options(), fx.NopLogger)
	if err := app.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stop, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if err := app.Stop(stop); err != nil {
			t.Error(err)
		}
	}()
	internal := keycloakToken(t, "wallet-internal", "wallet-internal-local-only")
	provider := keycloakToken(t, "provider-a", "provider-a-local-only")
	report, err := loadtest.Run(ctx, loadtest.Config{API: "http://" + address, InternalToken: internal, ProviderToken: provider, ProviderID: "provider-a", Requests: 120, Workers: 12, Wallets: 8})
	if err != nil {
		t.Fatal(err)
	}
	if report.Completed != 120 || report.Errors != 0 || report.StatusCounts[200] != 120 || report.P99Milliseconds < report.P50Milliseconds {
		t.Fatalf("unexpected load report: %+v", report)
	}
	var debits int64
	if err := f.conn.QueryRow(ctx, `SELECT count(*) FROM wallet_ledger_entry WHERE direction='DEBIT'`).Scan(&debits); err != nil || debits != 120 {
		t.Fatalf("load debits: %d %v", debits, err)
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("LOAD_REPORT_JSON=%s", data)
	command := exec.CommandContext(ctx, "go", "run", "../../cmd/loadtest", "-api", "http://"+address, "-token-url", os.Getenv("TEST_OIDC_TOKEN_URL"), "-requests", "20", "-workers", "4", "-wallets", "2")
	command.Env = append(os.Environ(), "LOAD_INTERNAL_CLIENT_SECRET=wallet-internal-local-only", "LOAD_PROVIDER_CLIENT_SECRET=provider-a-local-only")
	output, err := command.Output()
	if err != nil {
		t.Fatalf("load CLI: %v", err)
	}
	var cliReport loadtest.Report
	if err := json.Unmarshal(output, &cliReport); err != nil || cliReport.Completed != 20 || cliReport.Errors != 0 {
		t.Fatalf("load CLI report: %+v %v", cliReport, err)
	}
}
