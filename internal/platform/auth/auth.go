package auth

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"example.com/wagering/internal/application"
	"example.com/wagering/internal/config"
	"github.com/golang-jwt/jwt/v5"
	"go.uber.org/fx"
)

var errUnauthorized = errors.New("invalid access token")

type Claims struct {
	AuthorizedParty string `json:"azp"`
	TokenType       string `json:"typ"`
	jwt.RegisteredClaims
}
type Authenticator struct {
	issuer, jwksURL, audience, internal string
	providers                           map[string]bool
	client                              *http.Client
	mu                                  sync.RWMutex
	keys                                map[string]*rsa.PublicKey
	lastRefresh                         time.Time
	refreshMu                           sync.Mutex
}

func New(c config.Config) *Authenticator {
	providers := map[string]bool{}
	for _, id := range strings.Split(c.OIDCProviderClients, ",") {
		if id != "" {
			providers[strings.TrimSpace(id)] = true
		}
	}
	return &Authenticator{issuer: c.OIDCIssuer, jwksURL: c.OIDCJWKSURL, audience: c.OIDCAudience, internal: c.OIDCInternalClient, providers: providers, client: &http.Client{Timeout: 3 * time.Second}}
}
func (a *Authenticator) Enabled() bool                  { return a.issuer != "" }
func (a *Authenticator) ProviderAllowed(id string) bool { return a.providers[id] }
func (a *Authenticator) Initialize(ctx context.Context) error {
	if !a.Enabled() {
		return nil
	}
	return a.refresh(ctx)
}
func (a *Authenticator) Ready() error {
	if !a.Enabled() {
		return errors.New("OIDC is not configured")
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if len(a.keys) == 0 {
		return errors.New("OIDC keys unavailable")
	}
	return nil
}

type jwks struct {
	Keys []struct{ Kid, Kty, Alg, Use, N, E string } `json:"keys"`
}

func (a *Authenticator) refresh(ctx context.Context) error {
	a.refreshMu.Lock()
	defer a.refreshMu.Unlock()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.jwksURL, nil)
	if err != nil {
		return err
	}
	res, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return errors.New("OIDC JWKS unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, 1<<20+1))
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return errors.New("OIDC JWKS too large")
	}
	var set jwks
	if err = json.Unmarshal(data, &set); err != nil {
		return err
	}
	keys := map[string]*rsa.PublicKey{}
	for _, entry := range set.Keys {
		if entry.Kid == "" || entry.Kty != "RSA" || (entry.Alg != "" && entry.Alg != "RS256") || (entry.Use != "" && entry.Use != "sig") {
			continue
		}
		n, err := base64.RawURLEncoding.DecodeString(entry.N)
		if err != nil || len(n) < 256 || len(n) > 512 {
			continue
		}
		e, err := base64.RawURLEncoding.DecodeString(entry.E)
		if err != nil || len(e) == 0 || len(e) > 4 {
			continue
		}
		bigE := new(big.Int).SetBytes(e)
		if !bigE.IsInt64() || bigE.Int64() < 3 || bigE.Int64() > 2147483647 || bigE.Int64()%2 == 0 {
			continue
		}
		keys[entry.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(bigE.Int64())}
	}
	if len(keys) == 0 {
		return errors.New("OIDC JWKS has no RS256 keys")
	}
	a.mu.Lock()
	a.keys = keys
	a.lastRefresh = time.Now()
	a.mu.Unlock()
	return nil
}
func (a *Authenticator) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	a.mu.RLock()
	key := a.keys[kid]
	last := a.lastRefresh
	a.mu.RUnlock()
	if key != nil {
		return key, nil
	}
	if time.Since(last) > 5*time.Second {
		if err := a.refresh(ctx); err != nil {
			return nil, err
		}
		a.mu.RLock()
		key = a.keys[kid]
		a.mu.RUnlock()
	}
	if key == nil {
		return nil, errUnauthorized
	}
	return key, nil
}
func (a *Authenticator) Verify(ctx context.Context, raw string) (*Claims, error) {
	if !a.Enabled() || len(raw) > 16384 || raw == "" {
		return nil, errUnauthorized
	}
	claims := &Claims{}
	parser := jwt.NewParser(jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer(a.issuer), jwt.WithAudience(a.audience), jwt.WithExpirationRequired(), jwt.WithIssuedAt())
	token, err := parser.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		kid, ok := t.Header["kid"].(string)
		if !ok || kid == "" {
			return nil, errUnauthorized
		}
		return a.key(ctx, kid)
	})
	if err != nil || !token.Valid || claims.TokenType != "Bearer" || claims.AuthorizedParty == "" {
		return nil, errUnauthorized
	}
	return claims, nil
}
func writeError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code})
}
func (a *Authenticator) Wrap(next http.Handler, service *application.Service) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health/live" || r.URL.Path == "/health/ready" {
			next.ServeHTTP(w, r)
			return
		}
		if err := a.Ready(); err != nil {
			writeError(w, http.StatusServiceUnavailable, "AUTH_UNAVAILABLE")
			return
		}
		values := r.Header.Values("Authorization")
		if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED")
			return
		}
		claims, err := a.Verify(r.Context(), strings.TrimPrefix(values[0], "Bearer "))
		if err != nil {
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED")
			return
		}
		clientID := claims.AuthorizedParty
		internal := clientID == a.internal
		provider := a.providers[clientID]
		if !internal && !provider {
			writeError(w, http.StatusForbidden, "FORBIDDEN")
			return
		}
		path := r.URL.Path
		if path == "/wallets" || strings.HasPrefix(path, "/wallets/") || path == "/metrics" {
			if !internal {
				writeError(w, http.StatusForbidden, "FORBIDDEN")
				return
			}
		} else if path == "/wagering/transactions" && r.Method == http.MethodPost {
			if !provider {
				writeError(w, http.StatusForbidden, "FORBIDDEN")
				return
			}
			body, readErr := io.ReadAll(io.LimitReader(r.Body, 64*1024+1))
			if readErr != nil || len(body) > 64*1024 {
				writeError(w, http.StatusBadRequest, "INVALID_INPUT")
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			var operation struct {
				ProviderID string `json:"providerId"`
			}
			if json.Unmarshal(body, &operation) == nil && operation.ProviderID != "" && operation.ProviderID != clientID {
				writeError(w, http.StatusForbidden, "FORBIDDEN")
				return
			}
		} else if strings.HasPrefix(path, "/providers/") {
			parts := strings.Split(strings.Trim(path, "/"), "/")
			if !provider || len(parts) < 2 || parts[1] != clientID {
				writeError(w, http.StatusForbidden, "FORBIDDEN")
				return
			}
		} else if strings.HasPrefix(path, "/wagering/transactions/") {
			if !provider {
				writeError(w, http.StatusForbidden, "FORBIDDEN")
				return
			}
			id := strings.TrimPrefix(path, "/wagering/transactions/")
			ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			defer cancel()
			view, lookupErr := service.Transaction(ctx, id)
			if lookupErr != nil {
				switch {
				case errors.Is(lookupErr, application.ErrInvalidInput):
					writeError(w, http.StatusBadRequest, "INVALID_INPUT")
				case errors.Is(lookupErr, application.ErrNotFound):
					writeError(w, http.StatusNotFound, "NOT_FOUND")
				default:
					writeError(w, http.StatusServiceUnavailable, "TEMPORARILY_UNAVAILABLE")
				}
				return
			}
			if view.Operation.ProviderID != clientID {
				writeError(w, http.StatusForbidden, "FORBIDDEN")
				return
			}
		} else {
			writeError(w, http.StatusNotFound, "NOT_FOUND")
			return
		}
		next.ServeHTTP(w, r)
	})
}
func Register(lc fx.Lifecycle, a *Authenticator) {
	lc.Append(fx.Hook{OnStart: func(ctx context.Context) error { return a.Initialize(ctx) }})
}

var Module = fx.Module("auth", fx.Provide(New), fx.Invoke(Register))
