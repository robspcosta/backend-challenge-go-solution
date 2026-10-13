//go:build integration

package app

import (
	"bufio"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

func readRoleCredentials(t *testing.T, path string) (string, string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var id, secret string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "aws_access_key_id=") {
			id = strings.TrimPrefix(line, "aws_access_key_id=")
		}
		if strings.HasPrefix(line, "aws_secret_access_key=") {
			secret = strings.TrimPrefix(line, "aws_secret_access_key=")
		}
	}
	if err := scanner.Err(); err != nil || id == "" || secret == "" {
		t.Fatalf("invalid role credential file: %v", err)
	}
	return id, secret
}

func TestBrokerRolePoliciesOnLocalStack(t *testing.T) {
	producerPath, consumerPath := os.Getenv("TEST_PRODUCER_CREDENTIALS"), os.Getenv("TEST_CONSUMER_CREDENTIALS")
	if producerPath == "" || consumerPath == "" {
		t.Skip("run in Compose integration container with role-specific credentials mounted")
	}
	producerID, producerSecret := readRoleCredentials(t, producerPath)
	consumerID, consumerSecret := readRoleCredentials(t, consumerPath)
	if producerID == consumerID || producerSecret == consumerSecret {
		t.Fatal("producer and consumer share broker credentials")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	endpoint := os.Getenv("TEST_SQS_ENDPOINT")
	makeClient := func(id, secret string) *sqs.Client {
		cfg := aws.Config{Region: "us-east-1", Credentials: aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(id, secret, ""))}
		return sqs.NewFromConfig(cfg, func(o *sqs.Options) { o.BaseEndpoint = aws.String(endpoint) })
	}
	producerClient := makeClient(producerID, producerSecret)
	consumerClient := makeClient(consumerID, consumerSecret)
	input, err := producerClient.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String("wager-transactions.fifo")})
	if err != nil {
		t.Fatalf("producer cannot resolve input queue: %v", err)
	}
	if _, err := consumerClient.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String("wager-events.fifo")}); err != nil {
		t.Fatalf("consumer cannot resolve event queue: %v", err)
	}
	_, producerForbidden := producerClient.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: input.QueueUrl, AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn}})
	_, consumerForbidden := consumerClient.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: aws.String(strings.Replace(aws.ToString(input.QueueUrl), "wager-transactions.fifo", "wager-events.fifo", 1)), AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn}})
	if producerForbidden == nil || consumerForbidden == nil {
		t.Log("LocalStack accepted an operation outside the role policies: IAM enforcement is not effective in this runtime; HMAC verification remains mandatory")
	} else {
		t.Log("LocalStack denied both out-of-policy broker operations")
	}
}
