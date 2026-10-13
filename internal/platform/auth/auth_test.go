package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestTokenValidation(t *testing.T) {
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	a := &Authenticator{issuer: "http://issuer/realms/wagering", audience: "wagering-api", keys: map[string]*rsa.PublicKey{"key-1": &private.PublicKey}, lastRefresh: time.Now()}
	sign := func(c Claims) string {
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, c)
		token.Header["kid"] = "key-1"
		raw, err := token.SignedString(private)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	valid := Claims{AuthorizedParty: "provider-a", TokenType: "Bearer", RegisteredClaims: jwt.RegisteredClaims{Issuer: a.issuer, Audience: jwt.ClaimStrings{a.audience}, IssuedAt: jwt.NewNumericDate(time.Now().Add(-time.Second)), ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute))}}
	if _, err = a.Verify(context.Background(), sign(valid)); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*Claims){
		"expired":        func(c *Claims) { c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Minute)) },
		"wrong audience": func(c *Claims) { c.Audience = jwt.ClaimStrings{"other-api"} },
		"wrong issuer":   func(c *Claims) { c.Issuer = "http://another-issuer" },
		"ID token":       func(c *Claims) { c.TokenType = "ID" },
		"no expiration":  func(c *Claims) { c.ExpiresAt = nil },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			copy := valid
			change(&copy)
			if _, err := a.Verify(context.Background(), sign(copy)); err == nil {
				t.Fatal("invalid token accepted")
			}
		})
	}
	raw := sign(valid)
	parts := strings.Split(raw, ".")
	if parts[2][0] == 'A' {
		parts[2] = "B" + parts[2][1:]
	} else {
		parts[2] = "A" + parts[2][1:]
	}
	tampered := strings.Join(parts, ".")
	if _, err = a.Verify(context.Background(), tampered); err == nil {
		t.Fatal("tampered signature accepted")
	}
}
func TestMissingOIDCIsFailClosed(t *testing.T) {
	a := &Authenticator{}
	server := httptest.NewServer(a.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }), nil))
	defer server.Close()
	res, err := http.Get(server.URL + "/wallets")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 503 {
		t.Fatalf("status %d", res.StatusCode)
	}
}
