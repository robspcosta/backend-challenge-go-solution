//go:build integration

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"example.com/wagering/internal/config"
	"example.com/wagering/internal/platform/auth"
	"example.com/wagering/internal/platform/messageauth"
	"example.com/wagering/internal/platform/producer"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"go.uber.org/fx"
)

const testMessageSecret = "local-only-message-signing-key-change-me"

func brokerEnvelope(t *testing.T, walletID, playerID, id string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"messageId": id, "type": "WagerTransactionRequested", "occurredAt": time.Now().UTC().Format(time.RFC3339Nano),
		"data": map[string]any{"providerId": "provider-a", "externalTransactionId": id,
			"idempotencyKey": id, "playerId": playerID, "walletId": walletID,
			"roundId": "round-security", "gameId": "game-security", "kind": "BET",
			"money": map[string]string{"amount": "1.00", "currency": "BRL"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestUnsignedAndTamperedMessagesGoToRealDLQ(t *testing.T) {
	if os.Getenv("TEST_SQS_ENDPOINT") == "" {
		t.Skip("real LocalStack required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	client, inputURL, _ := recoverySQS(t, ctx)
	f := setupFinancial(t)
	w := newWallet(t, f, "100.00")
	configureRecoveryApp(t, f.pools[0].Config().ConnConfig.ConnString())
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
	unsigned := brokerEnvelope(t, w.ID, w.PlayerID, "unsigned-"+fmt.Sprint(time.Now().UnixNano()))
	tampered, err := messageauth.Sign(brokerEnvelope(t, w.ID, w.PlayerID, "tampered-"+fmt.Sprint(time.Now().UnixNano())), []byte(testMessageSecret))
	if err != nil {
		t.Fatal(err)
	}
	tampered = []byte(strings.Replace(string(tampered), `"amount":"1.00"`, `"amount":"2.00"`, 1))
	if _, err := messageauth.Verify(tampered, []byte(testMessageSecret)); err == nil {
		t.Fatal("tampered envelope still verifies")
	}
	for i, body := range [][]byte{unsigned, tampered} {
		id := fmt.Sprintf("security-%d-%d", time.Now().UnixNano(), i)
		if _, err := client.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: aws.String(inputURL), MessageBody: aws.String(string(body)), MessageGroupId: aws.String(w.ID), MessageDeduplicationId: aws.String(id)}); err != nil {
			t.Fatal(err)
		}
	}
	dlqName := strings.TrimSuffix(inputURL[strings.LastIndex(inputURL, "/")+1:], ".fifo") + "-dlq.fifo"
	dlq, err := client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(dlqName)})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	until := time.Now().Add(15 * time.Second)
	for len(seen) < 2 && time.Now().Before(until) {
		messages, err := client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: dlq.QueueUrl, WaitTimeSeconds: 1, MaxNumberOfMessages: 2})
		if err != nil {
			t.Fatal(err)
		}
		for _, msg := range messages.Messages {
			seen[aws.ToString(msg.Body)] = true
			if _, err := client.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: dlq.QueueUrl, ReceiptHandle: msg.ReceiptHandle}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !seen[string(unsigned)] || !seen[string(tampered)] {
		t.Fatalf("DLQ missing invalid messages: %d", len(seen))
	}
	var inbox, debits int
	if err := f.conn.QueryRow(ctx, `SELECT count(*) FROM inbox_message`).Scan(&inbox); err != nil {
		t.Fatal(err)
	}
	if err := f.conn.QueryRow(ctx, `SELECT count(*) FROM wallet_ledger_entry WHERE direction='DEBIT'`).Scan(&debits); err != nil {
		t.Fatal(err)
	}
	if inbox != 0 || debits != 0 {
		t.Fatalf("invalid messages affected finance: inbox=%d debits=%d", inbox, debits)
	}
}

func TestTrustedProducerUsesRealKeycloakAndSQS(t *testing.T) {
	if os.Getenv("TEST_SQS_ENDPOINT") == "" || os.Getenv("TEST_OIDC_TOKEN_URL") == "" {
		t.Skip("real LocalStack and Keycloak required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, inputURL, _ := recoverySQS(t, ctx)
	f := setupFinancial(t)
	w := newWallet(t, f, "100.00")
	a := auth.New(config.Config{OIDCIssuer: os.Getenv("TEST_OIDC_ISSUER"), OIDCJWKSURL: os.Getenv("TEST_OIDC_JWKS_URL"), OIDCAudience: "wagering-api", OIDCProviderClients: "provider-a,provider-b"})
	if err := a.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(&producer.Handler{Auth: a, SQS: client, QueueURL: inputURL, Secret: []byte(testMessageSecret)})
	defer server.Close()
	body := brokerEnvelope(t, w.ID, w.PlayerID, "producer-"+fmt.Sprint(time.Now().UnixNano()))
	request := func(token string, data []byte) int {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/wagering/messages", strings.NewReader(string(data)))
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		return res.StatusCode
	}
	if status := request("", body); status != http.StatusUnauthorized {
		t.Fatalf("missing token: %d", status)
	}
	providerB := keycloakToken(t, "provider-b", "provider-b-local-only")
	if status := request(providerB, body); status != http.StatusForbidden {
		t.Fatalf("wrong provider: %d", status)
	}
	providerA := keycloakToken(t, "provider-a", "provider-a-local-only")
	if status := request(providerA, body); status != http.StatusAccepted {
		t.Fatalf("authorized producer: %d", status)
	}
	response, err := client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(inputURL), WaitTimeSeconds: 1})
	if err != nil || len(response.Messages) != 1 {
		t.Fatalf("signed SQS message: %d %v", len(response.Messages), err)
	}
	if _, err := messageauth.Verify([]byte(aws.ToString(response.Messages[0].Body)), []byte(testMessageSecret)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(inputURL), ReceiptHandle: response.Messages[0].ReceiptHandle}); err != nil {
		t.Fatal(err)
	}
}
