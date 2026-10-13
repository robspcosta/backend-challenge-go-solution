package config

import "testing"

func TestLoad(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	if _, err := Load(); err == nil {
		t.Fatal("missing database accepted")
	}
	t.Setenv("DATABASE_URL", "postgres://local")
	t.Setenv("HTTP_ADDR", "")
	c, err := Load()
	if err != nil || c.HTTPAddress != ":8080" {
		t.Fatal(c, err)
	}
	t.Setenv("HTTP_ADDR", "invalid")
	if _, err := Load(); err == nil {
		t.Fatal("invalid address accepted")
	}
}

func TestOIDCConfiguration(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://local")
	t.Setenv("HTTP_ADDR", ":8080")
	t.Setenv("OIDC_ISSUER", "http://localhost:8081/realms/wagering")
	if _, err := Load(); err == nil {
		t.Fatal("incomplete OIDC accepted")
	}
	t.Setenv("OIDC_JWKS_URL", "http://localhost:8081/realms/wagering/protocol/openid-connect/certs")
	t.Setenv("OIDC_AUDIENCE", "wagering-api")
	t.Setenv("OIDC_INTERNAL_CLIENT", "wallet-internal")
	t.Setenv("OIDC_PROVIDER_CLIENTS", "provider-a,provider-b")
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OIDC_PROVIDER_CLIENTS", "provider-a,wallet-internal")
	if _, err := Load(); err == nil {
		t.Fatal("internal identity also mapped as provider")
	}
}

func TestSQSRequiresMessageSignatureSecret(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://local")
	t.Setenv("HTTP_ADDR", ":8080")
	t.Setenv("SQS_ENDPOINT", "http://localhost:4566")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("SQS_INPUT_QUEUE", "wager-transactions.fifo")
	t.Setenv("SQS_EVENT_QUEUE", "wager-events.fifo")
	t.Setenv("SQS_MESSAGE_HMAC_KEY", "")
	if _, err := Load(); err == nil {
		t.Fatal("unsigned SQS configuration accepted")
	}
	t.Setenv("SQS_MESSAGE_HMAC_KEY", "local-only-message-signing-key-change-me")
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
}
