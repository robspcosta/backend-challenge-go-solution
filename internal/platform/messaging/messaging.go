package messaging

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"example.com/wagering/internal/application"
	"example.com/wagering/internal/config"
	"example.com/wagering/internal/domain"
	"example.com/wagering/internal/platform/messageauth"
	"example.com/wagering/internal/platform/metrics"
	"example.com/wagering/internal/platform/repository"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"go.uber.org/fx"
)

type Transport struct {
	client                     *sqs.Client
	inputURL, eventURL, dlqURL string
	enabled                    bool
}

func NewTransport(c config.Config) (*Transport, error) {
	if c.SQSEndpoint == "" {
		return &Transport{}, nil
	}
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion(c.SQSRegion))
	if err != nil {
		return nil, err
	}
	client := sqs.NewFromConfig(cfg, func(o *sqs.Options) { o.BaseEndpoint = aws.String(c.SQSEndpoint) })
	return &Transport{client: client, enabled: true}, nil
}
func (t *Transport) initialize(ctx context.Context, c config.Config) error {
	in, err := t.client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(c.SQSInputQueue)})
	if err != nil {
		return err
	}
	out, err := t.client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(c.SQSEventQueue)})
	if err != nil {
		return err
	}
	t.inputURL = aws.ToString(in.QueueUrl)
	t.eventURL = aws.ToString(out.QueueUrl)
	dlqName := strings.TrimSuffix(c.SQSInputQueue, ".fifo") + "-dlq.fifo"
	dlq, err := t.client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(dlqName)})
	if err != nil {
		return err
	}
	t.dlqURL = aws.ToString(dlq.QueueUrl)
	return nil
}
func (t *Transport) Ready(ctx context.Context) error {
	if !t.enabled {
		return nil
	}
	_, err := t.client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: aws.String(t.inputURL), AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameApproximateNumberOfMessages}})
	return err
}

type envelope struct {
	MessageID  string    `json:"messageId"`
	Type       string    `json:"type"`
	OccurredAt time.Time `json:"occurredAt"`
	Data       struct {
		domain.Operation
		IdempotencyKey string `json:"idempotencyKey"`
	} `json:"data"`
}

func parse(body string) (envelope, error) {
	var e envelope
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		return e, err
	}
	if e.MessageID == "" || len(e.MessageID) > 256 || e.Type != "WagerTransactionRequested" || e.OccurredAt.IsZero() || e.Data.IdempotencyKey == "" {
		return e, application.ErrInvalidInput
	}
	if err := e.Data.Operation.Validate(); err != nil {
		return e, err
	}
	return e, nil
}

type Workers struct {
	transport *Transport
	service   *application.Service
	repo      *repository.Repository
	metrics   *metrics.Registry
	secret    []byte
	cancel    context.CancelFunc
	done      chan struct{}
}

func NewWorkers(t *Transport, s *application.Service, r *repository.Repository, m *metrics.Registry, c config.Config) *Workers {
	return &Workers{transport: t, service: s, repo: r, metrics: m, secret: []byte(c.SQSMessageHMACKey)}
}
func Register(lc fx.Lifecycle, c config.Config, w *Workers) {
	if !w.transport.enabled {
		return
	}
	lc.Append(fx.Hook{OnStart: func(ctx context.Context) error {
		if err := w.transport.initialize(ctx, c); err != nil {
			return err
		}
		workerCtx, cancel := context.WithCancel(context.Background())
		w.cancel = cancel
		w.done = make(chan struct{})
		go func() {
			defer close(w.done)
			var group sync.WaitGroup
			for i := 0; i < 3; i++ {
				group.Add(1)
				go func() { defer group.Done(); w.consume(workerCtx) }()
			}
			group.Add(1)
			go func() { defer group.Done(); w.publish(workerCtx) }()
			group.Add(1)
			go func() { defer group.Done(); w.observe(workerCtx) }()
			group.Wait()
		}()
		return nil
	}, OnStop: func(ctx context.Context) error {
		w.cancel()
		select {
		case <-w.done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}})
}
func (w *Workers) consume(ctx context.Context) {
	for ctx.Err() == nil {
		response, err := w.transport.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(w.transport.inputURL), MaxNumberOfMessages: 1, WaitTimeSeconds: 10, VisibilityTimeout: 30, MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameApproximateReceiveCount}})
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("SQS receive failed", "error", err)
				pause(ctx, time.Second)
			}
			continue
		}
		for i, msg := range response.Messages {
			if ctx.Err() != nil {
				for _, remaining := range response.Messages[i:] {
					w.release(remaining)
				}
				return
			}
			w.handle(ctx, msg)
		}
	}
}
func (w *Workers) handle(ctx context.Context, msg types.Message) {
	body := []byte(aws.ToString(msg.Body))
	if _, err := messageauth.Verify(body, w.secret); err != nil {
		w.deadLetter(ctx, msg)
		return
	}
	e, err := parse(aws.ToString(msg.Body))
	if err == nil {
		_, err = w.service.ProcessMessage(ctx, e.MessageID, []byte(aws.ToString(msg.Body)), e.Data.IdempotencyKey, e.Data.Operation)
	}
	if err != nil {
		w.metrics.ConsumerRetry()
		slog.Warn("SQS message not committed", "messageId", e.MessageID, "error", err)
		// The queue redrive policy sends invalid or repeatedly failing messages to the DLQ.
		// A short visibility release applies backoff without acknowledging the message.
		if ctx.Err() != nil {
			w.release(msg)
			return
		}
		n, _ := strconv.Atoi(msg.Attributes["ApproximateReceiveCount"])
		if n < 1 {
			n = 1
		}
		if n > 3 {
			n = 3
		}
		_, _ = w.transport.client.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{QueueUrl: aws.String(w.transport.inputURL), ReceiptHandle: msg.ReceiptHandle, VisibilityTimeout: int32(1 << n)})
		return
	}
	if ctx.Err() != nil {
		w.release(msg)
		return
	}
	_, err = w.transport.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(w.transport.inputURL), ReceiptHandle: msg.ReceiptHandle})
	if err != nil {
		slog.Error("SQS delete failed", "messageId", e.MessageID, "error", err)
	} else {
		w.metrics.ConsumerAcknowledged()
	}
}
func (w *Workers) deadLetter(ctx context.Context, msg types.Message) {
	body := aws.ToString(msg.Body)
	id := aws.ToString(msg.MessageId)
	if id == "" {
		sum := sha256.Sum256([]byte(body))
		id = hex.EncodeToString(sum[:])
	}
	_, err := w.transport.client.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: aws.String(w.transport.dlqURL), MessageBody: aws.String(body), MessageGroupId: aws.String("invalid-signature"), MessageDeduplicationId: aws.String(id)})
	if err != nil {
		slog.Error("SQS invalid message DLQ send failed", "sqsMessageId", id, "error", err)
		return
	}
	if _, err := w.transport.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(w.transport.inputURL), ReceiptHandle: msg.ReceiptHandle}); err != nil {
		slog.Error("SQS invalid message delete failed", "sqsMessageId", id, "error", err)
	}
}
func (w *Workers) release(msg types.Message) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = w.transport.client.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{QueueUrl: aws.String(w.transport.inputURL), ReceiptHandle: msg.ReceiptHandle, VisibilityTimeout: 0})
}
func (w *Workers) publish(ctx context.Context) {
	owner, err := application.NewID()
	if err != nil {
		return
	}
	for ctx.Err() == nil {
		rows, err := w.repo.ClaimEvents(ctx, owner, 10)
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("outbox claim failed", "error", err)
			}
			pause(ctx, time.Second)
			continue
		}
		if len(rows) == 0 {
			pause(ctx, 250*time.Millisecond)
			continue
		}
		for _, row := range rows {
			if ctx.Err() != nil {
				return
			}
			_, err = w.transport.client.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: aws.String(w.transport.eventURL), MessageBody: aws.String(string(row.Payload)), MessageGroupId: aws.String(row.AggregateID), MessageDeduplicationId: aws.String(row.EventID)})
			if err != nil {
				w.metrics.OutboxRetry()
				delay := time.Second << min(row.Attempts, 6)
				if saveErr := w.repo.RetryEvent(ctx, row.EventID, owner, delay, err.Error()); saveErr != nil {
					slog.Error("outbox retry save failed", "eventId", row.EventID, "error", saveErr)
				}
				continue
			}
			if err = w.repo.MarkPublished(ctx, row.EventID, owner); err != nil {
				slog.Error("outbox publish mark failed", "eventId", row.EventID, "error", err)
			}
		}
	}
}
func (w *Workers) observe(ctx context.Context) {
	for ctx.Err() == nil {
		queryCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		attributes, err := w.transport.client.GetQueueAttributes(queryCtx, &sqs.GetQueueAttributesInput{QueueUrl: aws.String(w.transport.dlqURL), AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameApproximateNumberOfMessages}})
		if err == nil {
			if n, parseErr := strconv.ParseFloat(attributes.Attributes[string(types.QueueAttributeNameApproximateNumberOfMessages)], 64); parseErr == nil {
				w.metrics.SetDLQDepth(n)
			}
		}
		if lag, queryErr := w.repo.OutboxLag(queryCtx); queryErr == nil {
			w.metrics.SetOutboxLag(lag)
		}
		cancel()
		pause(ctx, 5*time.Second)
	}
}
func pause(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}

var Module = fx.Module("messaging", fx.Provide(NewTransport, NewWorkers), fx.Invoke(Register))
