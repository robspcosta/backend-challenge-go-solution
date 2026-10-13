package application

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"time"

	"example.com/wagering/internal/domain"
)

type MessageIdentity struct {
	ID   string
	Hash [32]byte
}

func messageID(m *MessageIdentity) string {
	if m == nil {
		return ""
	}
	return m.ID
}

type Event struct {
	EventID       string    `json:"eventId"`
	EventType     string    `json:"eventType"`
	AggregateID   string    `json:"aggregateId"`
	CorrelationID string    `json:"correlationId"`
	CausationID   string    `json:"causationId,omitempty"`
	OccurredAt    time.Time `json:"occurredAt"`
	Version       int       `json:"version"`
	Data          any       `json:"data"`
}

type TransactionEventData struct {
	TransactionID string       `json:"transactionId"`
	WalletID      string       `json:"walletId"`
	ProviderID    string       `json:"providerId,omitempty"`
	Kind          string       `json:"kind,omitempty"`
	Money         domain.Money `json:"money"`
	Balance       domain.Money `json:"balance"`
	FailureCode   string       `json:"failureCode,omitempty"`
}
type BalanceChangedData struct {
	WalletID      string       `json:"walletId"`
	TransactionID string       `json:"transactionId"`
	Direction     string       `json:"direction"`
	Money         domain.Money `json:"money"`
	BalanceBefore domain.Money `json:"balanceBefore"`
	BalanceAfter  domain.Money `json:"balanceAfter"`
	WalletVersion int64        `json:"walletVersion"`
}

func recordEvents(ctx context.Context, tx Session, transactionID, walletID, providerID, kindOfOperation, correlationID, causationID, status, failure string, money, before, after domain.Money, direction string, version int64, at time.Time) error {
	if correlationID == "" {
		correlationID = transactionID
	}
	var kind string
	switch status {
	case "PROCESSED":
		kind = "WagerTransactionProcessed"
	case "REJECTED":
		kind = "WagerTransactionRejected"
	case "PENDING_REFERENCE":
		kind = "WagerTransactionPendingReference"
	default:
		return errors.New("unsupported event status")
	}
	data := TransactionEventData{TransactionID: transactionID, WalletID: walletID, ProviderID: providerID, Kind: kindOfOperation, Money: money, Balance: after, FailureCode: failure}
	if err := addEvent(ctx, tx, kind, walletID, correlationID, causationID, data, at); err != nil {
		return err
	}
	if status == "PROCESSED" && direction != "" {
		return addEvent(ctx, tx, "WalletBalanceChanged", walletID, correlationID, causationID, BalanceChangedData{walletID, transactionID, direction, money, before, after, version}, at)
	}
	return nil
}
func addEvent(ctx context.Context, tx Session, kind, aggregate, correlation, causation string, data any, at time.Time) error {
	id, err := NewID()
	if err != nil {
		return err
	}
	return tx.AddEvent(ctx, Event{EventID: id, EventType: kind, AggregateID: aggregate, CorrelationID: correlation, CausationID: causation, OccurredAt: at.UTC(), Version: 1, Data: data})
}

// ProcessMessage shares the same financial use case as HTTP. The inbox row is committed
// with the result and outbox; a duplicate with the same hash is safely acknowledged.
func (s *Service) ProcessMessage(ctx context.Context, messageID string, body []byte, key string, o domain.Operation) (Result, error) {
	if messageID == "" || len(messageID) > 256 {
		return Result{}, ErrInvalidInput
	}
	var canonical any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&canonical); err != nil {
		return Result{}, errors.Join(ErrInvalidInput, err)
	}
	b, err := json.Marshal(canonical)
	if err != nil {
		return Result{}, err
	}
	m := MessageIdentity{ID: messageID, Hash: sha256.Sum256(b)}
	return s.process(ctx, key, o, &m)
}
