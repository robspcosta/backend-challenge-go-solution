package messageauth

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"

	"example.com/wagering/internal/domain"
)

var ErrInvalidSignature = errors.New("invalid message signature")

type Envelope struct {
	MessageID  string `json:"messageId"`
	Type       string `json:"type"`
	OccurredAt string `json:"occurredAt"`
	Data       struct {
		domain.Operation
		IdempotencyKey string `json:"idempotencyKey"`
	} `json:"data"`
}

type signature struct {
	ProviderID  string `json:"providerId"`
	MessageID   string `json:"messageId"`
	PayloadHash string `json:"payloadHash"`
	MAC         string `json:"mac"`
}

func parseObject(body []byte) (map[string]json.RawMessage, error) {
	var value map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil || value == nil {
		return nil, ErrInvalidSignature
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, ErrInvalidSignature
	}
	return value, nil
}

func digest(value map[string]json.RawMessage) (string, error) {
	delete(value, "signature")
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	var canonical any
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err := decoder.Decode(&canonical); err != nil {
		return "", err
	}
	encoded, err = json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func mac(secret []byte, providerID, messageID, payloadHash string) string {
	input, _ := json.Marshal([3]string{providerID, messageID, payloadHash})
	h := hmac.New(sha256.New, secret)
	_, _ = h.Write(input)
	return hex.EncodeToString(h.Sum(nil))
}

// Sign binds the complete unsigned envelope, including the idempotency key,
// to the authenticated provider and the durable message identity.
func Sign(body []byte, secret []byte) ([]byte, error) {
	if len(secret) < 32 {
		return nil, ErrInvalidSignature
	}
	value, err := parseObject(body)
	if err != nil {
		return nil, err
	}
	if _, exists := value["signature"]; exists {
		return nil, ErrInvalidSignature
	}
	var envelope Envelope
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.MessageID == "" || envelope.Data.ProviderID == "" {
		return nil, ErrInvalidSignature
	}
	hash, err := digest(value)
	if err != nil {
		return nil, err
	}
	sig := signature{ProviderID: envelope.Data.ProviderID, MessageID: envelope.MessageID, PayloadHash: hash}
	sig.MAC = mac(secret, sig.ProviderID, sig.MessageID, sig.PayloadHash)
	encoded, err := json.Marshal(sig)
	if err != nil {
		return nil, err
	}
	value["signature"] = encoded
	return json.Marshal(value)
}

func Verify(body []byte, secret []byte) (Envelope, error) {
	var envelope Envelope
	if len(secret) < 32 {
		return envelope, ErrInvalidSignature
	}
	value, err := parseObject(body)
	if err != nil {
		return envelope, err
	}
	raw, ok := value["signature"]
	if !ok {
		return envelope, ErrInvalidSignature
	}
	var sig signature
	if err := json.Unmarshal(raw, &sig); err != nil || sig.ProviderID == "" || sig.MessageID == "" || len(sig.PayloadHash) != 64 || len(sig.MAC) != 64 {
		return envelope, ErrInvalidSignature
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.MessageID != sig.MessageID || envelope.Data.ProviderID != sig.ProviderID {
		return Envelope{}, ErrInvalidSignature
	}
	hash, err := digest(value)
	if err != nil || !hmac.Equal([]byte(hash), []byte(sig.PayloadHash)) || !hmac.Equal([]byte(mac(secret, sig.ProviderID, sig.MessageID, hash)), []byte(sig.MAC)) {
		return Envelope{}, ErrInvalidSignature
	}
	return envelope, nil
}
