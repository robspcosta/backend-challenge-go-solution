package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"example.com/wagering/internal/config"
	"example.com/wagering/internal/platform/auth"
	"example.com/wagering/internal/platform/producer"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

func run(ctx context.Context) error {
	secret := os.Getenv("SQS_MESSAGE_HMAC_KEY")
	endpoint := os.Getenv("SQS_ENDPOINT")
	region := os.Getenv("AWS_REGION")
	queue := os.Getenv("SQS_INPUT_QUEUE")
	address := os.Getenv("PRODUCER_HTTP_ADDR")
	if len(secret) < 32 || endpoint == "" || region == "" || queue == "" || os.Getenv("OIDC_ISSUER") == "" || os.Getenv("OIDC_JWKS_URL") == "" || os.Getenv("OIDC_AUDIENCE") == "" || os.Getenv("OIDC_PROVIDER_CLIENTS") == "" {
		return errors.New("producer configuration incomplete")
	}
	if address == "" {
		address = ":8082"
	}
	a := auth.New(config.Config{OIDCIssuer: os.Getenv("OIDC_ISSUER"), OIDCJWKSURL: os.Getenv("OIDC_JWKS_URL"), OIDCAudience: os.Getenv("OIDC_AUDIENCE"), OIDCProviderClients: os.Getenv("OIDC_PROVIDER_CLIENTS")})
	if err := a.Initialize(ctx); err != nil {
		return err
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return err
	}
	client := sqs.NewFromConfig(cfg, func(o *sqs.Options) { o.BaseEndpoint = aws.String(endpoint) })
	url, err := client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(queue)})
	if err != nil {
		return err
	}
	server := &http.Server{Addr: address, Handler: &producer.Handler{Auth: a, SQS: client, QueueURL: aws.ToString(url.QueueUrl), Secret: []byte(secret)}, ReadHeaderTimeout: 5 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	}
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("producer stopped", "error", err)
		os.Exit(1)
	}
}
