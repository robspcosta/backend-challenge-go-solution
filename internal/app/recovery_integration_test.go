//go:build integration

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"example.com/wagering/internal/application"
	"example.com/wagering/internal/domain"
	"example.com/wagering/internal/platform/messageauth"
	"example.com/wagering/internal/platform/metrics"
	"example.com/wagering/internal/platform/repository"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"
)

func recoverySQS(t *testing.T, ctx context.Context) (*sqs.Client, string, string) {
	t.Helper()
	endpoint := os.Getenv("TEST_SQS_ENDPOINT")
	if endpoint == "" {
		t.Skip("set TEST_SQS_ENDPOINT for real LocalStack integration")
	}
	configuration, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion("us-east-1"))
	if err != nil {
		t.Fatal(err)
	}
	client := sqs.NewFromConfig(configuration, func(options *sqs.Options) { options.BaseEndpoint = aws.String(endpoint) })
	id, err := application.NewID()
	if err != nil {
		t.Fatal(err)
	}
	base := "wager-recovery-" + id[:8]
	inputName, dlqName, eventName := base+".fifo", base+"-dlq.fifo", base+"-events.fifo"
	dlq, err := client.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String(dlqName), Attributes: map[string]string{"FifoQueue": "true"}})
	if err != nil {
		t.Fatal(err)
	}
	dlqAttributes, err := client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: dlq.QueueUrl, AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn}})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := json.Marshal(map[string]string{"deadLetterTargetArn": dlqAttributes.Attributes[string(types.QueueAttributeNameQueueArn)], "maxReceiveCount": "5"})
	if err != nil {
		t.Fatal(err)
	}
	input, err := client.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String(inputName), Attributes: map[string]string{"FifoQueue": "true", "RedrivePolicy": string(policy)}})
	if err != nil {
		t.Fatal(err)
	}
	events, err := client.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String(eventName), Attributes: map[string]string{"FifoQueue": "true"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SQS_INPUT_QUEUE", inputName)
	t.Setenv("SQS_EVENT_QUEUE", eventName)
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		for _, url := range []*string{input.QueueUrl, events.QueueUrl, dlq.QueueUrl} {
			if _, err := client.DeleteQueue(cleanup, &sqs.DeleteQueueInput{QueueUrl: url}); err != nil {
				t.Error(err)
			}
		}
	})
	return client, aws.ToString(input.QueueUrl), aws.ToString(events.QueueUrl)
}
func configureRecoveryApp(t *testing.T, dsn string) {
	t.Helper()
	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("HTTP_ADDR", "127.0.0.1:0")
	t.Setenv("SQS_ENDPOINT", os.Getenv("TEST_SQS_ENDPOINT"))
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("SQS_MESSAGE_HMAC_KEY", "local-only-message-signing-key-change-me")
}
func waitRecovery(t *testing.T, until time.Time, check func() (bool, error)) {
	t.Helper()
	for time.Now().Before(until) {
		ok, err := check()
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("recovery did not finish before deadline")
}

// This helper is launched as a separate test process and killed by its parent.
func TestConsumerCommitCrashHelper(t *testing.T) {
	if os.Getenv("WAGER_CONSUMER_CRASH_HELPER") != "1" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	configuration, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion("us-east-1"))
	if err != nil {
		t.Fatal(err)
	}
	client := sqs.NewFromConfig(configuration, func(options *sqs.Options) { options.BaseEndpoint = aws.String(os.Getenv("TEST_SQS_ENDPOINT")) })
	queue, err := client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(os.Getenv("SQS_INPUT_QUEUE"))})
	if err != nil {
		t.Fatal(err)
	}
	inputURL := aws.ToString(queue.QueueUrl)
	messageID := os.Getenv("WAGER_CRASH_MESSAGE_ID")
	for ctx.Err() == nil {
		received, err := client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(inputURL), WaitTimeSeconds: 1, VisibilityTimeout: 10})
		if err != nil {
			t.Fatal(err)
		}
		for _, msg := range received.Messages {
			var envelope struct {
				MessageID string `json:"messageId"`
				Data      struct {
					domain.Operation
					IdempotencyKey string `json:"idempotencyKey"`
				} `json:"data"`
			}
			if err := json.Unmarshal([]byte(aws.ToString(msg.Body)), &envelope); err != nil || envelope.MessageID != messageID {
				_, _ = client.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{QueueUrl: aws.String(inputURL), ReceiptHandle: msg.ReceiptHandle, VisibilityTimeout: 0})
				continue
			}
			service := application.New(repository.New(pool))
			result, err := service.ProcessMessage(ctx, envelope.MessageID, []byte(aws.ToString(msg.Body)), envelope.Data.IdempotencyKey, envelope.Data.Operation)
			if err != nil || result.Status != "PROCESSED" {
				t.Fatalf("commit: %+v %v", result, err)
			}
			// Short visibility leaves a bounded gap for the parent to kill this process.
			if _, err := client.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{QueueUrl: aws.String(inputURL), ReceiptHandle: msg.ReceiptHandle, VisibilityTimeout: 3}); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(os.Getenv("WAGER_COMMIT_MARKER"), []byte(result.TransactionID), 0600); err != nil {
				t.Fatal(err)
			}
			<-ctx.Done() // The parent kills us before DeleteMessage.
			return
		}
	}
	t.Fatal("target message not received")
}

func TestConsumerKilledAfterCommitBeforeDelete(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	client, inputURL, _ := recoverySQS(t, ctx)
	f := setupFinancial(t)
	w := newWallet(t, f, "100.00")
	o := op(t, w, fmt.Sprintf("crash-bet-%d", time.Now().UnixNano()), "BET", "25.00")
	messageID := fmt.Sprintf("crash-message-%d", time.Now().UnixNano())
	key := "crash-key-" + messageID
	envelope := map[string]any{"messageId": messageID, "type": "WagerTransactionRequested", "occurredAt": time.Now().UTC().Format(time.RFC3339Nano), "data": map[string]any{"providerId": o.ProviderID, "externalTransactionId": o.ExternalTransactionID, "idempotencyKey": key, "playerId": o.PlayerID, "walletId": o.WalletID, "roundId": o.RoundID, "gameId": o.GameID, "kind": o.Kind, "money": o.Money}}
	body, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	body, err = messageauth.Sign(body, []byte(os.Getenv("SQS_MESSAGE_HMAC_KEY")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: aws.String(inputURL), MessageBody: aws.String(string(body)), MessageGroupId: aws.String(w.ID), MessageDeduplicationId: aws.String(messageID)}); err != nil {
		t.Fatal(err)
	}
	marker := t.TempDir() + "/committed"
	child := exec.Command(os.Args[0], "-test.run=^TestConsumerCommitCrashHelper$", "-test.v")
	var childOutput bytes.Buffer
	child.Stdout = &childOutput
	child.Stderr = &childOutput
	child.Env = append(os.Environ(), "WAGER_CONSUMER_CRASH_HELPER=1", "WAGER_CRASH_MESSAGE_ID="+messageID, "WAGER_COMMIT_MARKER="+marker, "DATABASE_URL="+f.pools[0].Config().ConnConfig.ConnString())
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	killed := false
	t.Cleanup(func() {
		if !killed {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	})
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if child.ProcessState != nil && child.ProcessState.Exited() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(marker); err != nil {
		_ = child.Process.Kill()
		_ = child.Wait()
		killed = true
		t.Fatalf("child did not commit: %s", childOutput.String())
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	killed = true
	var inbox, debits int
	if err := f.conn.QueryRow(ctx, `SELECT count(*) FROM inbox_message WHERE message_id=$1`, messageID).Scan(&inbox); err != nil || inbox != 1 {
		t.Fatalf("committed inbox: %d %v", inbox, err)
	}
	if err := f.conn.QueryRow(ctx, `SELECT count(*) FROM wallet_ledger_entry WHERE wallet_id=$1 AND direction='DEBIT'`, w.ID).Scan(&debits); err != nil || debits != 1 {
		t.Fatalf("committed debit: %d %v", debits, err)
	}
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
	waitRecovery(t, time.Now().Add(15*time.Second), func() (bool, error) {
		recorder := httptest.NewRecorder()
		meter.ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
		return strings.Contains(recorder.Body.String(), "wager_duplicates_total 1") && strings.Contains(recorder.Body.String(), "wager_consumer_acknowledged_total 1"), nil
	})
	if err := f.conn.QueryRow(ctx, `SELECT count(*) FROM wallet_ledger_entry WHERE wallet_id=$1 AND direction='DEBIT'`, w.ID).Scan(&debits); err != nil || debits != 1 {
		t.Fatalf("duplicate debit: %d %v", debits, err)
	}
	replay, err := f.services[2].Process(ctx, key, o)
	if err != nil || !replay.IdempotentReplay || replay.Balance.Cents() != 7500 {
		t.Fatalf("replay after crash: %+v %v", replay, err)
	}
}

func TestPublisherRecoversAfterSendBeforeMark(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, _, eventsURL := recoverySQS(t, ctx)
	f := setupFinancial(t)
	w := newWallet(t, f, "100.00")
	owner, _ := application.NewID()
	claimed, err := repository.New(f.pools[0]).ClaimEvents(ctx, owner, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %+v %v", claimed, err)
	}
	row := claimed[0]
	// The send succeeds, then this publisher disappears before MarkPublished.
	if _, err := client.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: aws.String(eventsURL), MessageBody: aws.String(string(row.Payload)), MessageGroupId: aws.String(w.ID), MessageDeduplicationId: aws.String(row.EventID)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.conn.Exec(ctx, `UPDATE outbox_event SET lease_until=now()-interval '1 second' WHERE event_id=$1`, row.EventID); err != nil {
		t.Fatal(err)
	}
	configureRecoveryApp(t, f.pools[1].Config().ConnConfig.ConnString())
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
	waitRecovery(t, time.Now().Add(15*time.Second), func() (bool, error) {
		var published bool
		var attempts int
		err := f.conn.QueryRow(ctx, `SELECT published_at IS NOT NULL,attempts FROM outbox_event WHERE event_id=$1`, row.EventID).Scan(&published, &attempts)
		return published && attempts >= 2, err
	})
	// FIFO deduplication may suppress the second delivery, so stable event identity is asserted in SQL.
	var payload []byte
	if err := f.conn.QueryRow(ctx, `SELECT payload FROM outbox_event WHERE event_id=$1`, row.EventID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var event application.Event
	if err := json.Unmarshal(payload, &event); err != nil || event.EventID != row.EventID {
		t.Fatalf("event identity: %+v %v", event, err)
	}
}

func TestHTTPCommitReplayedBySQS(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	client, inputURL, _ := recoverySQS(t, ctx)
	f := setupFinancial(t)
	w := newWallet(t, f, "100.00")
	o := op(t, w, fmt.Sprintf("http-then-sqs-%d", time.Now().UnixNano()), "BET", "25.00")
	key := "cross-channel-" + o.ExternalTransactionID
	status, raw, err := send(f.servers[0].URL, "POST", "/wagering/transactions", key, o)
	if err != nil || status != 200 {
		t.Fatalf("HTTP commit: %d %s %v", status, raw, err)
	}
	var original application.Result
	if err := json.Unmarshal(raw, &original); err != nil || original.Balance.Cents() != 7500 {
		t.Fatalf("HTTP result: %+v %v", original, err)
	}
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
	messageID := fmt.Sprintf("cross-channel-message-%d", time.Now().UnixNano())
	envelope := map[string]any{"messageId": messageID, "type": "WagerTransactionRequested", "occurredAt": time.Now().UTC().Format(time.RFC3339Nano), "data": map[string]any{"providerId": o.ProviderID, "externalTransactionId": o.ExternalTransactionID, "idempotencyKey": key, "playerId": o.PlayerID, "walletId": o.WalletID, "roundId": o.RoundID, "gameId": o.GameID, "kind": o.Kind, "money": o.Money}}
	body, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	body, err = messageauth.Sign(body, []byte(os.Getenv("SQS_MESSAGE_HMAC_KEY")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: aws.String(inputURL), MessageBody: aws.String(string(body)), MessageGroupId: aws.String(w.ID), MessageDeduplicationId: aws.String(messageID)}); err != nil {
		t.Fatal(err)
	}
	waitRecovery(t, time.Now().Add(12*time.Second), func() (bool, error) {
		var count int
		err := f.conn.QueryRow(ctx, `SELECT count(*) FROM inbox_message WHERE message_id=$1`, messageID).Scan(&count)
		return count == 1, err
	})
	waitRecovery(t, time.Now().Add(5*time.Second), func() (bool, error) {
		recorder := httptest.NewRecorder()
		meter.ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
		return strings.Contains(recorder.Body.String(), "wager_consumer_acknowledged_total 1"), nil
	})
	var debits, transactions int
	if err := f.conn.QueryRow(ctx, `SELECT count(*) FROM wallet_ledger_entry WHERE wallet_id=$1 AND direction='DEBIT'`, w.ID).Scan(&debits); err != nil || debits != 1 {
		t.Fatalf("debits: %d %v", debits, err)
	}
	if err := f.conn.QueryRow(ctx, `SELECT count(*) FROM wager_transaction WHERE wallet_id=$1 AND external_transaction_id=$2`, w.ID, o.ExternalTransactionID).Scan(&transactions); err != nil || transactions != 1 {
		t.Fatalf("transactions: %d %v", transactions, err)
	}
	replayed, err := f.services[2].Process(ctx, key, o)
	if err != nil || !replayed.IdempotentReplay || replayed.TransactionID != original.TransactionID || replayed.Balance.Cents() != 7500 {
		t.Fatalf("cross-channel replay: %+v %v", replayed, err)
	}
}
