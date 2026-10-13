//go:build integration

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"example.com/wagering/internal/application"
	"example.com/wagering/internal/platform/messageauth"
	"example.com/wagering/internal/platform/metrics"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"go.uber.org/fx"
)

func metricValue(t *testing.T, meter *metrics.Registry, name string) int {
	t.Helper()
	recorder := httptest.NewRecorder()
	meter.ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	for _, line := range strings.Split(recorder.Body.String(), "\n") {
		if value, ok := strings.CutPrefix(line, name+" "); ok {
			n, err := strconv.Atoi(value)
			if err != nil {
				t.Fatal(err)
			}
			return n
		}
	}
	t.Fatalf("metric %s not found", name)
	return 0
}

// The wallet row is held by a separate transaction until the SQS consumer and the HTTP
// handler both wait on it, so both channels pass the idempotency lookup before either commits.
func TestConcurrentHTTPAndSQSSameOperation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	client, inputURL, _ := recoverySQS(t, ctx)
	f := setupFinancial(t)
	configureRecoveryApp(t, f.pools[1].Config().ConnConfig.ConnString())
	var meter *metrics.Registry
	app := fx.New(Options(), fx.Populate(&meter), fx.NopLogger)
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
	for round, sqsFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("sqsFirst=%t", sqsFirst), func(t *testing.T) {
			w := newWallet(t, f, "100.00")
			o := op(t, w, fmt.Sprintf("concurrent-channel-%d-%d", round, time.Now().UnixNano()), "BET", "25.00")
			key := "concurrent-key-" + o.ExternalTransactionID
			messageID := "concurrent-message-" + o.ExternalTransactionID
			envelope := map[string]any{"messageId": messageID, "type": "WagerTransactionRequested", "occurredAt": time.Now().UTC().Format(time.RFC3339Nano), "data": map[string]any{"providerId": o.ProviderID, "externalTransactionId": o.ExternalTransactionID, "idempotencyKey": key, "playerId": o.PlayerID, "walletId": o.WalletID, "roundId": o.RoundID, "gameId": o.GameID, "kind": o.Kind, "money": o.Money}}
			body, err := json.Marshal(envelope)
			if err != nil {
				t.Fatal(err)
			}
			body, err = messageauth.Sign(body, []byte(os.Getenv("SQS_MESSAGE_HMAC_KEY")))
			if err != nil {
				t.Fatal(err)
			}
			duplicatesBefore := metricValue(t, meter, "wager_duplicates_total")
			acknowledgedBefore := metricValue(t, meter, "wager_consumer_acknowledged_total")

			blocker, err := f.pools[2].Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = blocker.Rollback(context.Background()) }()
			var blockerPID int32
			if err = blocker.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&blockerPID); err != nil {
				t.Fatal(err)
			}
			if _, err = blocker.Exec(ctx, "SELECT 1 FROM wallet WHERE id=$1 FOR UPDATE", w.ID); err != nil {
				t.Fatal(err)
			}
			// A second waiter queues behind the first one's tuple lock, so it is counted by the
			// LockWallet statement it waits on rather than by its direct blocker.
			waitBlocked := func(expected int) {
				waitRecovery(t, time.Now().Add(10*time.Second), func() (bool, error) {
					var waiting int
					err := f.conn.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE pid<>$1 AND wait_event_type='Lock' AND query LIKE '%FROM wallet WHERE id=$1 FOR UPDATE'`, blockerPID).Scan(&waiting)
					return waiting >= expected, err
				})
			}
			var httpCode int
			var httpData []byte
			var httpErr error
			httpDone := make(chan struct{})
			fireHTTP := func() {
				go func() {
					defer close(httpDone)
					httpCode, httpData, httpErr = send(f.servers[0].URL, "POST", "/wagering/transactions", key, o)
				}()
			}
			fireSQS := func() {
				if _, err := client.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: aws.String(inputURL), MessageBody: aws.String(string(body)), MessageGroupId: aws.String(w.ID), MessageDeduplicationId: aws.String(messageID)}); err != nil {
					t.Fatal(err)
				}
			}
			first, second := fireSQS, fireHTTP
			if !sqsFirst {
				first, second = fireHTTP, fireSQS
			}
			first()
			waitBlocked(1)
			second()
			waitBlocked(2)
			if err = blocker.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			<-httpDone

			var httpResult application.Result
			if httpErr == nil {
				httpErr = json.Unmarshal(httpData, &httpResult)
			}
			if httpErr != nil || httpCode != 200 || httpResult.Status != "PROCESSED" || httpResult.Balance.Cents() != 7500 {
				t.Fatalf("HTTP outcome: %d %s %v", httpCode, httpData, httpErr)
			}
			waitRecovery(t, time.Now().Add(15*time.Second), func() (bool, error) {
				var inbox int
				err := f.conn.QueryRow(ctx, `SELECT count(*) FROM inbox_message WHERE message_id=$1`, messageID).Scan(&inbox)
				return inbox == 1 && metricValue(t, meter, "wager_consumer_acknowledged_total") == acknowledgedBefore+1, err
			})
			sqsReplay := metricValue(t, meter, "wager_duplicates_total") - duplicatesBefore
			if sqsReplay != 0 && sqsReplay != 1 {
				t.Fatalf("SQS duplicates delta: %d", sqsReplay)
			}
			if httpResult.IdempotentReplay == (sqsReplay == 1) {
				t.Fatalf("exactly one channel must apply the operation: httpReplay=%t sqsReplay=%t", httpResult.IdempotentReplay, sqsReplay == 1)
			}
			winner, replayed := "HTTP", "SQS"
			if httpResult.IdempotentReplay {
				winner, replayed = "SQS", "HTTP"
			}
			t.Logf("applied by %s, replayed by %s", winner, replayed)

			var transactionID string
			var transactions int
			if err := f.conn.QueryRow(ctx, `SELECT min(id::text),count(*) FROM wager_transaction WHERE provider_id=$1 AND external_transaction_id=$2`, o.ProviderID, o.ExternalTransactionID).Scan(&transactionID, &transactions); err != nil || transactions != 1 || transactionID != httpResult.TransactionID {
				t.Fatalf("transactions: %d %s %v", transactions, transactionID, err)
			}
			assertWallet(t, f, w, 7500, 2, 1)
			code, data, err := send(f.servers[1].URL, "POST", "/wagering/transactions", key, o)
			var replay application.Result
			if err == nil {
				err = json.Unmarshal(data, &replay)
			}
			if err != nil || code != 200 || !replay.IdempotentReplay || replay.TransactionID != transactionID || replay.Balance.Cents() != 7500 {
				t.Fatalf("replay after race: %d %s %v", code, data, err)
			}
		})
	}
}
