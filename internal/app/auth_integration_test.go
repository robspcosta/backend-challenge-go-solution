//go:build integration

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"example.com/wagering/internal/application"
	"example.com/wagering/internal/config"
	"example.com/wagering/internal/platform/auth"
	"example.com/wagering/internal/platform/httpapi"
	"go.uber.org/fx"
)

func keycloakToken(t *testing.T, clientID, secret string) string {
	t.Helper()
	values := url.Values{"grant_type": {"client_credentials"}, "client_id": {clientID}, "client_secret": {secret}}
	res, err := http.PostForm(os.Getenv("TEST_OIDC_TOKEN_URL"), values)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var body struct {
		AccessToken string `json:"access_token"`
	}
	if err = json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 || body.AccessToken == "" {
		t.Fatalf("token endpoint: %d", res.StatusCode)
	}
	return body.AccessToken
}
func authorizedRequest(t *testing.T, base, method, path, token, key string, body any) (int, []byte) {
	t.Helper()
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, base+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
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
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	result, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, result
}
func TestKeycloakProviderIsolation(t *testing.T) {
	if os.Getenv("TEST_OIDC_TOKEN_URL") == "" {
		t.Skip("set TEST_OIDC_TOKEN_URL for real Keycloak integration")
	}
	f := setupFinancial(t)
	c := config.Config{OIDCIssuer: os.Getenv("TEST_OIDC_ISSUER"), OIDCJWKSURL: os.Getenv("TEST_OIDC_JWKS_URL"), OIDCAudience: "wagering-api", OIDCInternalClient: "wallet-internal", OIDCProviderClients: "provider-a,provider-b"}
	a := auth.New(c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	httpapi.New(f.services[0]).Register(mux)
	server := httptest.NewServer(a.Wrap(mux, f.services[0]))
	defer server.Close()
	internal := keycloakToken(t, "wallet-internal", "wallet-internal-local-only")
	providerA := keycloakToken(t, "provider-a", "provider-a-local-only")
	providerB := keycloakToken(t, "provider-b", "provider-b-local-only")
	playerID, err := application.NewID()
	if err != nil {
		t.Fatal(err)
	}
	walletBody := map[string]any{"playerId": playerID, "initialBalance": m(t, "100.00")}
	status, _ := authorizedRequest(t, server.URL, "POST", "/wallets", "", "", walletBody)
	if status != 401 {
		t.Fatalf("missing token: %d", status)
	}
	status, _ = authorizedRequest(t, server.URL, "POST", "/wallets", "invalid", "", walletBody)
	if status != 401 {
		t.Fatalf("invalid token: %d", status)
	}
	status, _ = authorizedRequest(t, server.URL, "POST", "/wallets", providerA, "", walletBody)
	if status != 403 {
		t.Fatalf("provider opened wallet: %d", status)
	}
	status, raw := authorizedRequest(t, server.URL, "POST", "/wallets", internal, "", walletBody)
	if status != 201 {
		t.Fatalf("internal wallet: %d %s", status, raw)
	}
	var wallet application.WalletView
	if err = json.Unmarshal(raw, &wallet); err != nil {
		t.Fatal(err)
	}
	operation := op(t, wallet, fmt.Sprintf("auth-bet-%d", time.Now().UnixNano()), "BET", "25.00")
	status, _ = authorizedRequest(t, server.URL, "POST", "/wagering/transactions", providerB, "auth-key", operation)
	if status != 403 {
		t.Fatalf("wrong provider wrote: %d", status)
	}
	status, _ = authorizedRequest(t, server.URL, "POST", "/wagering/transactions", internal, "auth-key", operation)
	if status != 403 {
		t.Fatalf("internal submitted provider transaction: %d", status)
	}
	status, raw = authorizedRequest(t, server.URL, "POST", "/wagering/transactions", providerA, "auth-key", operation)
	if status != 200 {
		t.Fatalf("authorized wager: %d %s", status, raw)
	}
	var result application.Result
	if err = json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.Balance.Cents() != 7500 {
		t.Fatal(result)
	}
	status, raw = authorizedRequest(t, server.URL, "POST", "/wagering/transactions", providerA, "auth-key", operation)
	if status != 200 {
		t.Fatalf("authorized replay: %d %s", status, raw)
	}
	if err = json.Unmarshal(raw, &result); err != nil || !result.IdempotentReplay {
		t.Fatalf("replay: %+v %v", result, err)
	}
	status, _ = authorizedRequest(t, server.URL, "GET", "/wagering/transactions/"+result.TransactionID, providerB, "", nil)
	if status != 403 {
		t.Fatalf("foreign transaction by id: %d", status)
	}
	status, _ = authorizedRequest(t, server.URL, "GET", "/providers/provider-a/wagering/transactions/"+operation.ExternalTransactionID, providerB, "", nil)
	if status != 403 {
		t.Fatalf("foreign provider path: %d", status)
	}
	status, _ = authorizedRequest(t, server.URL, "GET", "/wallets/"+wallet.ID, providerA, "", nil)
	if status != 403 {
		t.Fatalf("provider read wallet: %d", status)
	}
	status, _ = authorizedRequest(t, server.URL, "GET", "/wallets/"+wallet.ID, internal, "", nil)
	if status != 200 {
		t.Fatalf("internal wallet read: %d", status)
	}
	status, _ = authorizedRequest(t, server.URL, "GET", "/wagering/transactions/"+result.TransactionID, providerA, "", nil)
	if status != 200 {
		t.Fatalf("own transaction read: %d", status)
	}
	parts := strings.Split(providerA, ".")
	if parts[2][0] == 'A' {
		parts[2] = "B" + parts[2][1:]
	} else {
		parts[2] = "A" + parts[2][1:]
	}
	tampered := strings.Join(parts, ".")
	status, _ = authorizedRequest(t, server.URL, "GET", "/wagering/transactions/"+result.TransactionID, tampered, "", nil)
	if status != 401 {
		t.Fatalf("tampered signature: %d", status)
	}
	var ledger int
	if err = f.conn.QueryRow(ctx, `SELECT count(*) FROM wallet_ledger_entry WHERE wallet_id=$1`, wallet.ID).Scan(&ledger); err != nil || ledger != 2 {
		t.Fatalf("unauthorized effect: %d %v", ledger, err)
	}
	// The same OIDC configuration must also start and stop through the production Fx graph.
	t.Setenv("DATABASE_URL", f.pools[0].Config().ConnConfig.ConnString())
	t.Setenv("HTTP_ADDR", "127.0.0.1:0")
	t.Setenv("OIDC_ISSUER", c.OIDCIssuer)
	t.Setenv("OIDC_JWKS_URL", c.OIDCJWKSURL)
	t.Setenv("OIDC_AUDIENCE", c.OIDCAudience)
	t.Setenv("OIDC_INTERNAL_CLIENT", c.OIDCInternalClient)
	t.Setenv("OIDC_PROVIDER_CLIENTS", c.OIDCProviderClients)
	app := fx.New(Options(), fx.NopLogger)
	if err = app.Start(ctx); err != nil {
		t.Fatal(err)
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopCancel()
	if err = app.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
}
