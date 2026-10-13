//go:build integration

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"example.com/wagering/internal/application"
	"example.com/wagering/internal/platform/messageauth"
	"example.com/wagering/internal/platform/repository"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"go.uber.org/fx"
)

func TestInboxOutboxAtomicityAndRecovery(t *testing.T) {
	f := setupFinancial(t)
	w := newWallet(t, f, "100.00")
	o := op(t, w, "messaging-bet", "BET", "25.00")
	body := []byte(`{"messageId":"message-1","type":"WagerTransactionRequested","data":{"idempotencyKey":"bet-key"}}`)
	var wg sync.WaitGroup
	errs := make(chan error, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := f.services[i%3].ProcessMessage(context.Background(), "message-1", body, "bet-key", o)
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var inbox, ledger, outbox int
	if err := f.conn.QueryRow(context.Background(), `SELECT count(*) FROM inbox_message`).Scan(&inbox); err != nil || inbox != 1 {
		t.Fatalf("inbox %d %v", inbox, err)
	}
	if err := f.conn.QueryRow(context.Background(), `SELECT count(*) FROM wallet_ledger_entry WHERE wallet_id=$1`, w.ID).Scan(&ledger); err != nil || ledger != 2 {
		t.Fatalf("ledger %d %v", ledger, err)
	}
	if err := f.conn.QueryRow(context.Background(), `SELECT count(*) FROM outbox_event WHERE aggregate_id=$1`, w.ID).Scan(&outbox); err != nil || outbox != 4 {
		t.Fatalf("outbox %d %v", outbox, err)
	}
	changed := append([]byte(nil), body...)
	changed = append(changed, ' ')
	// Whitespace is normalized before hashing, but a changed envelope is a collision.
	_, err := f.services[0].ProcessMessage(context.Background(), "message-1", changed, "bet-key", o)
	if err != nil {
		t.Fatal(err)
	}
	changed = []byte(`{"messageId":"message-1","type":"WagerTransactionRequested","data":{"idempotencyKey":"other-key"}}`)
	_, err = f.services[0].ProcessMessage(context.Background(), "message-1", changed, "bet-key", o)
	if !errors.Is(err, application.ErrConflict) {
		t.Fatalf("expected collision, got %v", err)
	}
	repoA := repository.New(f.pools[0])
	repoB := repository.New(f.pools[1])
	ownerA, _ := application.NewID()
	ownerB, _ := application.NewID()
	var a, b []repository.OutboxRow
	var aErr, bErr error
	wg.Add(2)
	go func() { defer wg.Done(); a, aErr = repoA.ClaimEvents(context.Background(), ownerA, 2) }()
	go func() { defer wg.Done(); b, bErr = repoB.ClaimEvents(context.Background(), ownerB, 2) }()
	wg.Wait()
	if aErr != nil || bErr != nil {
		t.Fatal(aErr, bErr)
	}
	seen := map[string]bool{}
	for _, row := range append(a, b...) {
		if seen[row.EventID] {
			t.Fatal("duplicate claim")
		}
		seen[row.EventID] = true
	}
	if len(seen) != 4 {
		t.Fatalf("claimed %d events", len(seen))
	}
	if _, err = f.conn.Exec(context.Background(), `UPDATE outbox_event SET payload='{}' WHERE event_id=$1`, a[0].EventID); err == nil {
		t.Fatal("mutable outbox payload")
	}
	if _, err = f.conn.Exec(context.Background(), `DELETE FROM outbox_event WHERE event_id=$1`, a[0].EventID); err == nil {
		t.Fatal("deletable outbox event")
	}
	if _, err = f.conn.Exec(context.Background(), `TRUNCATE outbox_event`); err == nil {
		t.Fatal("truncatable outbox")
	}
	if _, err = f.conn.Exec(context.Background(), `TRUNCATE inbox_message`); err == nil {
		t.Fatal("truncatable inbox")
	}
	if _, err = f.conn.Exec(context.Background(), `UPDATE outbox_event SET lease_until=now()-interval '1 second' WHERE event_id=$1`, a[0].EventID); err != nil {
		t.Fatal(err)
	}
	recovered, err := repoB.ClaimEvents(context.Background(), ownerB, 1)
	if err != nil || len(recovered) != 1 || recovered[0].EventID != a[0].EventID {
		t.Fatalf("lease recovery: %+v %v", recovered, err)
	}
	var wire map[string]any
	if err = json.Unmarshal(recovered[0].Payload, &wire); err != nil || wire["eventId"] != a[0].EventID {
		t.Fatalf("stable event identity: %v %+v", err, wire)
	}
	if err = repoB.MarkPublished(context.Background(), recovered[0].EventID, ownerB); err != nil {
		t.Fatal(err)
	}
	if err = repoB.RetryEvent(context.Background(), b[0].EventID, ownerB, 2*time.Second, "temporary SQS outage"); err != nil {
		t.Fatal(err)
	}
	var attempts int
	var next time.Time
	if err = f.conn.QueryRow(context.Background(), `SELECT attempts,next_attempt_at FROM outbox_event WHERE event_id=$1`, b[0].EventID).Scan(&attempts, &next); err != nil || attempts != 1 || !next.After(time.Now()) {
		t.Fatalf("retry scheduling: %d %v %v", attempts, next, err)
	}
}

func TestMessagingMigrationRoundTrip(t *testing.T) {
	f := setupFinancial(t)
	ctx := context.Background()
	down, err := os.ReadFile("../../migrations/000003_messaging.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.conn.Exec(ctx, string(down)); err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err = f.conn.QueryRow(ctx, `SELECT to_regclass('outbox_event') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatalf("down migration: %v %v", exists, err)
	}
	up, err := os.ReadFile("../../migrations/000003_messaging.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.conn.Exec(ctx, string(up)); err != nil {
		t.Fatal(err)
	}
	if err = f.conn.QueryRow(ctx, `SELECT to_regclass('outbox_event') IS NOT NULL`).Scan(&exists); err != nil || !exists {
		t.Fatalf("up migration: %v %v", exists, err)
	}
}

func TestOutboxEventPolicy(t *testing.T) {
	f := setupFinancial(t)
	zero := newWallet(t, f, "0.00")
	w := newWallet(t, f, "100.00")
	count := func(wallet, kind string) int {
		var n int
		err := f.conn.QueryRow(context.Background(), `SELECT count(*) FROM outbox_event WHERE aggregate_id=$1 AND event_type=$2`, wallet, kind).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count(zero.ID, "WagerTransactionProcessed") != 0 || count(w.ID, "WagerTransactionProcessed") != 1 || count(w.ID, "WalletBalanceChanged") != 1 {
		t.Fatal("opening event policy")
	}
	loss := op(t, w, "loss-event", "LOSS", "0.00")
	mustProcess(t, f, "loss-event", loss, "PROCESSED")
	if count(w.ID, "WagerTransactionProcessed") != 2 || count(w.ID, "WalletBalanceChanged") != 1 {
		t.Fatal("LOSS changed balance")
	}
	rejected := op(t, w, "rejected-event", "BET", "200.00")
	mustProcess(t, f, "rejected-event", rejected, "REJECTED")
	if count(w.ID, "WagerTransactionRejected") != 1 || count(w.ID, "WalletBalanceChanged") != 1 {
		t.Fatal("rejected transaction changed balance")
	}
	refund := op(t, w, "pending-refund-event", "REFUND", "10.00")
	refund.ReferenceExternalTransactionID = "late-bet-event"
	mustProcess(t, f, "pending-refund-event", refund, "PENDING_REFERENCE")
	if count(w.ID, "WagerTransactionPendingReference") != 1 {
		t.Fatal("missing pending event")
	}
	bet := op(t, w, "late-bet-event", "BET", "10.00")
	mustProcess(t, f, "late-bet-event", bet, "PROCESSED")
	time.Sleep(300 * time.Millisecond)
	if _, err := f.services[0].ProcessDue(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	if count(w.ID, "WagerTransactionProcessed") != 4 || count(w.ID, "WalletBalanceChanged") != 3 {
		t.Fatal("reference completion events missing")
	}
}

func TestSQSConsumerAndPublisher(t *testing.T) {
	endpoint := os.Getenv("TEST_SQS_ENDPOINT")
	if endpoint == "" {
		t.Skip("set TEST_SQS_ENDPOINT for real LocalStack integration")
	}
	f := setupFinancial(t)
	w := newWallet(t, f, "100.00")
	t.Setenv("DATABASE_URL", f.pools[0].Config().ConnConfig.ConnString())
	t.Setenv("HTTP_ADDR", "127.0.0.1:0")
	t.Setenv("SQS_ENDPOINT", endpoint)
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("SQS_INPUT_QUEUE", "wager-transactions.fifo")
	t.Setenv("SQS_EVENT_QUEUE", "wager-events.fifo")
	t.Setenv("SQS_MESSAGE_HMAC_KEY", "local-only-message-signing-key-change-me")
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion("us-east-1"))
	if err != nil {
		t.Fatal(err)
	}
	client := sqs.NewFromConfig(cfg, func(o *sqs.Options) { o.BaseEndpoint = aws.String(endpoint) })
	input, err := client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String("wager-transactions.fifo")})
	if err != nil {
		t.Fatal(err)
	}
	events, err := client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String("wager-events.fifo")})
	if err != nil {
		t.Fatal(err)
	}
	dlq, err := client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String("wager-transactions-dlq.fifo")})
	if err != nil {
		t.Fatal(err)
	}
	app := fx.New(Options(), fx.NopLogger)
	if err = app.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		if err := app.Stop(stopCtx); err != nil {
			t.Error(err)
		}
	}()
	o := op(t, w, fmt.Sprintf("sqs-%d", time.Now().UnixNano()), "BET", "25.00")
	messageID := fmt.Sprintf("msg-%d", time.Now().UnixNano())
	request := map[string]any{"messageId": messageID, "type": "WagerTransactionRequested", "occurredAt": time.Now().UTC().Format(time.RFC3339Nano), "data": map[string]any{"providerId": o.ProviderID, "externalTransactionId": o.ExternalTransactionID, "idempotencyKey": o.ExternalTransactionID, "playerId": o.PlayerID, "walletId": o.WalletID, "roundId": o.RoundID, "gameId": o.GameID, "kind": o.Kind, "money": o.Money}}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err = messageauth.Sign(body, []byte(os.Getenv("SQS_MESSAGE_HMAC_KEY")))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: input.QueueUrl, MessageBody: aws.String(string(body)), MessageGroupId: aws.String(w.ID), MessageDeduplicationId: aws.String(messageID)})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTicker(100 * time.Millisecond)
	defer deadline.Stop()
	for {
		var count int
		err = f.conn.QueryRow(ctx, `SELECT count(*) FROM inbox_message WHERE message_id=$1`, messageID).Scan(&count)
		if err != nil {
			t.Fatal(err)
		}
		if count == 1 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("message not consumed", ctx.Err())
		case <-deadline.C:
		}
	}
	var ledger int
	if err = f.conn.QueryRow(ctx, `SELECT count(*) FROM wallet_ledger_entry WHERE wallet_id=$1`, w.ID).Scan(&ledger); err != nil || ledger != 2 {
		t.Fatalf("ledger %d %v", ledger, err)
	}
	replay, err := f.services[0].Process(ctx, o.ExternalTransactionID, o)
	if err != nil || !replay.IdempotentReplay || replay.Balance.Cents() != 7500 {
		t.Fatalf("HTTP/SQS replay: %+v %v", replay, err)
	}
	for {
		var published int
		err = f.conn.QueryRow(ctx, `SELECT count(*) FROM outbox_event WHERE aggregate_id=$1 AND published_at IS NOT NULL`, w.ID).Scan(&published)
		if err != nil {
			t.Fatal(err)
		}
		if published == 4 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("events not published", ctx.Err())
		case <-deadline.C:
		}
	}
	seen := map[string]bool{}
	for len(seen) < 4 {
		res, receiveErr := client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: events.QueueUrl, WaitTimeSeconds: 1, MaxNumberOfMessages: 10})
		if receiveErr != nil {
			t.Fatal(receiveErr)
		}
		for _, msg := range res.Messages {
			var event application.Event
			if err = json.Unmarshal([]byte(aws.ToString(msg.Body)), &event); err != nil {
				t.Fatal(err)
			}
			if event.AggregateID == w.ID {
				seen[event.EventID] = true
				_, _ = client.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: events.QueueUrl, ReceiptHandle: msg.ReceiptHandle})
			}
		}
		if ctx.Err() != nil {
			t.Fatal("events not received", ctx.Err())
		}
	}
	// Re-delivery after a committed inbox row is harmless.
	_, err = client.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: input.QueueUrl, MessageBody: aws.String(string(body)), MessageGroupId: aws.String(w.ID), MessageDeduplicationId: aws.String(messageID + "-redelivery")})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if err = f.conn.QueryRow(ctx, `SELECT count(*) FROM wallet_ledger_entry WHERE wallet_id=$1`, w.ID).Scan(&ledger); err != nil || ledger != 2 {
		t.Fatalf("redelivery ledger %d %v", ledger, err)
	}
	badID := fmt.Sprintf("invalid-%d", time.Now().UnixNano())
	_, err = client.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: input.QueueUrl, MessageBody: aws.String("invalid-json-" + badID), MessageGroupId: aws.String(badID), MessageDeduplicationId: aws.String(badID)})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for !found {
		res, receiveErr := client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: dlq.QueueUrl, WaitTimeSeconds: 2})
		if receiveErr != nil {
			t.Fatal(receiveErr)
		}
		for _, msg := range res.Messages {
			if aws.ToString(msg.Body) == "invalid-json-"+badID {
				found = true
				_, _ = client.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: dlq.QueueUrl, ReceiptHandle: msg.ReceiptHandle})
			}
		}
		if ctx.Err() != nil {
			t.Fatal("invalid message did not reach DLQ", ctx.Err())
		}
	}
}
