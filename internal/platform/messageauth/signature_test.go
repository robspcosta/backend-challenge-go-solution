package messageauth

import (
	"strings"
	"testing"
)

func TestSignatureBindsIdentityAndPayload(t *testing.T) {
	secret := []byte("local-only-message-signing-key-change-me")
	body := []byte(`{"messageId":"message-1","type":"WagerTransactionRequested","occurredAt":"2026-10-12T12:00:00Z","data":{"providerId":"provider-a","idempotencyKey":"key-1","money":{"amount":"1.00","currency":"BRL"}}}`)
	signed, err := Sign(body, secret)
	if err != nil {
		t.Fatal(err)
	}
	if envelope, err := Verify(signed, secret); err != nil || envelope.MessageID != "message-1" {
		t.Fatalf("valid signature: %+v %v", envelope, err)
	}
	for _, changed := range [][]byte{
		body,
		[]byte(strings.Replace(string(signed), `"provider-a"`, `"provider-b"`, 1)),
		[]byte(strings.Replace(string(signed), `"message-1"`, `"message-2"`, 1)),
		[]byte(strings.Replace(string(signed), `"1.00"`, `"2.00"`, 1)),
		[]byte(strings.Replace(string(signed), `"key-1"`, `"key-2"`, 1)),
	} {
		if _, err := Verify(changed, secret); err == nil {
			t.Fatalf("accepted altered or unsigned body: %s", changed)
		}
	}
}
