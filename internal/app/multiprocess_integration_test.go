//go:build integration

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"example.com/wagering/internal/application"
)

func freeAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := l.Addr().String()
	_ = l.Close()
	return address
}
func requestWithToken(base, method, path, token, key string, body any) (int, []byte, error) {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
	}
	req, err := http.NewRequest(method, base+path, bytes.NewReader(data))
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	data, err = io.ReadAll(res.Body)
	return res.StatusCode, data, err
}
func TestThreeIndependentProcesses(t *testing.T) {
	if os.Getenv("TEST_OIDC_TOKEN_URL") == "" {
		t.Skip("set TEST_OIDC_TOKEN_URL for real Keycloak integration")
	}
	f := setupFinancial(t)
	binary := filepath.Join(t.TempDir(), "wager-server")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.Command("go", "build", "-race", "-o", binary, "../../cmd/server")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build child: %v %s", err, output)
	}
	bases := make([]string, 3)
	for i := 0; i < 3; i++ {
		address := freeAddress(t)
		bases[i] = "http://" + address
		cmd := exec.Command(binary)
		var logs bytes.Buffer
		cmd.Stdout = &logs
		cmd.Stderr = &logs
		cmd.Env = append(os.Environ(),
			"DATABASE_URL="+f.pools[i].Config().ConnConfig.ConnString(), "HTTP_ADDR="+address, "SQS_ENDPOINT=",
			"OIDC_ISSUER="+os.Getenv("TEST_OIDC_ISSUER"), "OIDC_JWKS_URL="+os.Getenv("TEST_OIDC_JWKS_URL"),
			"OIDC_AUDIENCE=wagering-api", "OIDC_INTERNAL_CLIENT=wallet-internal", "OIDC_PROVIDER_CLIENTS=provider-a,provider-b")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		deadline := time.Now().Add(20 * time.Second)
		for {
			res, err := http.Get(bases[i] + "/health/ready")
			if err == nil {
				_ = res.Body.Close()
				if res.StatusCode == 200 {
					break
				}
			}
			if time.Now().After(deadline) {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				t.Fatalf("child %d not ready: %s", i, logs.String())
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	internal := keycloakToken(t, "wallet-internal", "wallet-internal-local-only")
	provider := keycloakToken(t, "provider-a", "provider-a-local-only")
	create := func(base string) application.WalletView {
		player, _ := application.NewID()
		code, raw, err := requestWithToken(base, "POST", "/wallets", internal, "", map[string]any{"playerId": player, "initialBalance": m(t, "100.00")})
		if err != nil || code != 201 {
			t.Fatalf("create wallet: %d %s %v", code, raw, err)
		}
		var wallet application.WalletView
		if err := json.Unmarshal(raw, &wallet); err != nil {
			t.Fatal(err)
		}
		return wallet
	}
	w := create(bases[0])
	independent := create(bases[2])
	type answer struct {
		status int
		result application.Result
		err    error
	}
	answers := make([]answer, 3)
	var group sync.WaitGroup
	start := make(chan struct{})
	for i := range answers {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			<-start
			wallet := w
			amount := "80.00"
			if i == 2 {
				wallet = independent
				amount = "10.00"
			}
			operation := op(t, wallet, fmt.Sprintf("process-bet-%d", i), "BET", amount)
			status, raw, err := requestWithToken(bases[i], "POST", "/wagering/transactions", provider, fmt.Sprintf("process-key-%d", i), operation)
			answers[i].status = status
			answers[i].err = err
			if err == nil {
				answers[i].err = json.Unmarshal(raw, &answers[i].result)
			}
		}(i)
	}
	close(start)
	group.Wait()
	processed, rejected := 0, 0
	for i := 0; i < 2; i++ {
		if answers[i].err != nil {
			t.Fatal(answers[i].err)
		}
		switch answers[i].result.Status {
		case "PROCESSED":
			processed++
		case "REJECTED":
			rejected++
		default:
			t.Fatalf("unexpected result: %+v", answers[i])
		}
	}
	if processed != 1 || rejected != 1 || answers[2].err != nil || answers[2].result.Status != "PROCESSED" {
		t.Fatalf("cross-process results: %+v", answers)
	}
	ctx := context.Background()
	actual, err := f.services[0].Wallet(ctx, w.ID)
	if err != nil || actual.Balance.Cents() != 2000 {
		t.Fatalf("shared wallet: %+v %v", actual, err)
	}
	actual, err = f.services[0].Wallet(ctx, independent.ID)
	if err != nil || actual.Balance.Cents() != 9000 {
		t.Fatalf("independent wallet: %+v %v", actual, err)
	}
	var debits int
	if err := f.conn.QueryRow(ctx, `SELECT count(*) FROM wallet_ledger_entry WHERE wallet_id=$1 AND direction='DEBIT'`, w.ID).Scan(&debits); err != nil || debits != 1 {
		t.Fatalf("debits: %d %v", debits, err)
	}
	sharedOperation := op(t, w, "process-repeat-50", "BET", "10.00")
	repeated := make([]answer, 50)
	start = make(chan struct{})
	for i := range repeated {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			<-start
			status, raw, err := requestWithToken(bases[i%3], "POST", "/wagering/transactions", provider, "process-repeat-key", sharedOperation)
			repeated[i].status, repeated[i].err = status, err
			if err == nil {
				repeated[i].err = json.Unmarshal(raw, &repeated[i].result)
			}
		}(i)
	}
	close(start)
	group.Wait()
	firstID, fresh := "", 0
	for _, result := range repeated {
		if result.err != nil || result.status != 200 || result.result.Status != "PROCESSED" || result.result.Balance.Cents() != 1000 {
			t.Fatalf("repeat: %+v", result)
		}
		if firstID == "" {
			firstID = result.result.TransactionID
		}
		if result.result.TransactionID != firstID {
			t.Fatal("different transaction IDs on replay")
		}
		if !result.result.IdempotentReplay {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatalf("fresh transactions: %d", fresh)
	}
	actual, err = f.services[0].Wallet(ctx, w.ID)
	if err != nil || actual.Balance.Cents() != 1000 {
		t.Fatalf("repeat balance: %+v %v", actual, err)
	}
	if err := f.conn.QueryRow(ctx, `SELECT count(*) FROM wallet_ledger_entry WHERE wallet_id=$1 AND direction='DEBIT'`, w.ID).Scan(&debits); err != nil || debits != 2 {
		t.Fatalf("repeat debits: %d %v", debits, err)
	}
	status, raw, err := requestWithToken(bases[0], "GET", "/metrics", provider, "", nil)
	if err != nil || status != 403 {
		t.Fatalf("provider metrics access: %d %s %v", status, raw, err)
	}
	status, raw, err = requestWithToken(bases[0], "GET", "/metrics", internal, "", nil)
	if err != nil || status != 200 || !bytes.Contains(raw, []byte("wager_results_total")) {
		t.Fatalf("internal metrics: %d %s %v", status, raw, err)
	}
}
