package producer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"example.com/wagering/internal/platform/auth"
	"example.com/wagering/internal/platform/messageauth"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

type Handler struct {
	Auth     *auth.Authenticator
	SQS      *sqs.Client
	QueueURL string
	Secret   []byte
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/wagering/messages" {
		http.NotFound(w, r)
		return
	}
	if err := h.Auth.Ready(); err != nil {
		http.Error(w, "auth unavailable", http.StatusServiceUnavailable)
		return
	}
	parts := strings.Fields(r.Header.Get("Authorization"))
	if len(parts) != 2 || parts[0] != "Bearer" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	claims, err := h.Auth.Verify(r.Context(), parts[1])
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !h.Auth.ProviderAllowed(claims.AuthorizedParty) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024+1))
	if err != nil || len(body) > 64*1024 {
		http.Error(w, "invalid message", http.StatusBadRequest)
		return
	}
	var envelope messageauth.Envelope
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Type != "WagerTransactionRequested" || envelope.MessageID == "" || len(envelope.MessageID) > 256 || envelope.Data.IdempotencyKey == "" || len(envelope.Data.IdempotencyKey) > 256 || envelope.Data.Operation.Validate() != nil {
		http.Error(w, "invalid message", http.StatusBadRequest)
		return
	}
	if envelope.Data.ProviderID != claims.AuthorizedParty {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if at, err := time.Parse(time.RFC3339Nano, envelope.OccurredAt); err != nil || at.IsZero() {
		http.Error(w, "invalid message", http.StatusBadRequest)
		return
	}
	signed, err := messageauth.Sign(body, h.Secret)
	if err != nil {
		http.Error(w, "invalid message", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	_, err = h.SQS.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: aws.String(h.QueueURL), MessageBody: aws.String(string(signed)), MessageGroupId: aws.String(envelope.Data.WalletID), MessageDeduplicationId: aws.String(envelope.MessageID)})
	if err != nil {
		http.Error(w, "SQS unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"messageId": envelope.MessageID, "status": "ACCEPTED"})
}
