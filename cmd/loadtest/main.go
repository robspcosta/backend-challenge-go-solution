package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"example.com/wagering/internal/loadtest"
)

func token(ctx context.Context, client *http.Client, endpoint, id, secret string) (string, error) {
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {id}, "client_secret": {secret}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Body = io.NopCloser(strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint for %s returned HTTP %d", id, res.StatusCode)
	}
	var payload struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 64*1024)).Decode(&payload); err != nil {
		return "", err
	}
	if payload.AccessToken == "" {
		return "", errors.New("token endpoint returned no access token")
	}
	return payload.AccessToken, nil
}
func environment(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
func run() error {
	api := flag.String("api", environment("LOAD_API_URL", "http://127.0.0.1:8080"), "API base URL")
	tokenURL := flag.String("token-url", environment("LOAD_TOKEN_URL", "http://127.0.0.1:8081/realms/wagering/protocol/openid-connect/token"), "OIDC token URL")
	requests := flag.Int("requests", 200, "number of BET requests")
	workers := flag.Int("workers", 16, "parallel request workers")
	wallets := flag.Int("wallets", 8, "distinct test wallets")
	provider := flag.String("provider", environment("LOAD_PROVIDER_ID", "provider-a"), "provider client ID")
	flag.Parse()
	internalSecret := os.Getenv("LOAD_INTERNAL_CLIENT_SECRET")
	providerSecret := os.Getenv("LOAD_PROVIDER_CLIENT_SECRET")
	if internalSecret == "" || providerSecret == "" {
		return errors.New("LOAD_INTERNAL_CLIENT_SECRET and LOAD_PROVIDER_CLIENT_SECRET are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	client := &http.Client{Timeout: 12 * time.Second}
	internalToken, err := token(ctx, client, *tokenURL, "wallet-internal", internalSecret)
	if err != nil {
		return err
	}
	providerToken, err := token(ctx, client, *tokenURL, *provider, providerSecret)
	if err != nil {
		return err
	}
	report, err := loadtest.Run(ctx, loadtest.Config{API: *api, InternalToken: internalToken, ProviderToken: providerToken, ProviderID: *provider, Requests: *requests, Workers: *workers, Wallets: *wallets, Client: client})
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(report)
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
