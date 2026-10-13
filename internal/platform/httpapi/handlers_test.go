package httpapi

import (
	"example.com/wagering/internal/application"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInvalidRequests(t *testing.T) {
	mux := http.NewServeMux()
	New(application.New(nil)).Register(mux)
	for _, tc := range []struct{ method, path, body, media, key string }{
		{"POST", "/wallets", "{}", "text/plain", ""},
		{"POST", "/wallets", `{"playerId":"x","unknown":1}`, "application/json", ""},
		{"POST", "/wallets", "{} {}", "application/json", ""},
		{"POST", "/wagering/transactions", "{}", "application/json", ""},
		{"POST", "/wagering/transactions", `{"money":{"amount":25,"currency":"BRL"}}`, "application/json", "key"},
		{"GET", "/wallets/invalid", "", "", ""},
		{"GET", "/wallets/invalid/ledger?limit=-1", "", "", ""},
	} {
		request := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		request.Header.Set("Content-Type", tc.media)
		if tc.key != "" {
			request.Header.Set("Idempotency-Key", tc.key)
		}
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, request)
		if recorder.Code != 400 {
			t.Fatalf("%s %s: %d", tc.method, tc.path, recorder.Code)
		}
	}
}
