package config

import (
	"errors"
	"go.uber.org/fx"
	"net"
	"net/url"
	"os"
	"strings"
)

type Config struct {
	DatabaseURL, HTTPAddress                                                       string
	SQSEndpoint, SQSRegion, SQSInputQueue, SQSEventQueue                           string
	SQSMessageHMACKey                                                              string
	OIDCIssuer, OIDCJWKSURL, OIDCAudience, OIDCInternalClient, OIDCProviderClients string
}

func Load() (Config, error) {
	c := Config{DatabaseURL: os.Getenv("DATABASE_URL"), HTTPAddress: os.Getenv("HTTP_ADDR"), SQSEndpoint: os.Getenv("SQS_ENDPOINT"), SQSRegion: os.Getenv("AWS_REGION"), SQSInputQueue: os.Getenv("SQS_INPUT_QUEUE"), SQSEventQueue: os.Getenv("SQS_EVENT_QUEUE"), SQSMessageHMACKey: os.Getenv("SQS_MESSAGE_HMAC_KEY"), OIDCIssuer: os.Getenv("OIDC_ISSUER"), OIDCJWKSURL: os.Getenv("OIDC_JWKS_URL"), OIDCAudience: os.Getenv("OIDC_AUDIENCE"), OIDCInternalClient: os.Getenv("OIDC_INTERNAL_CLIENT"), OIDCProviderClients: os.Getenv("OIDC_PROVIDER_CLIENTS")}
	if c.DatabaseURL == "" {
		return Config{}, errors.New("DATABASE_URL is required")
	}
	if c.HTTPAddress == "" {
		c.HTTPAddress = ":8080"
	}
	if _, _, err := net.SplitHostPort(c.HTTPAddress); err != nil {
		return Config{}, errors.New("invalid HTTP_ADDR")
	}
	if c.SQSEndpoint != "" && (c.SQSRegion == "" || c.SQSInputQueue == "" || c.SQSEventQueue == "" || len(c.SQSMessageHMACKey) < 32) {
		return Config{}, errors.New("SQS configuration incomplete")
	}
	configured := 0
	for _, value := range []string{c.OIDCIssuer, c.OIDCJWKSURL, c.OIDCAudience, c.OIDCInternalClient, c.OIDCProviderClients} {
		if value != "" {
			configured++
		}
	}
	if configured != 0 && configured != 5 {
		return Config{}, errors.New("OIDC configuration incomplete")
	}
	if configured == 5 {
		for _, raw := range []string{c.OIDCIssuer, c.OIDCJWKSURL, c.OIDCAudience, c.OIDCInternalClient, c.OIDCProviderClients} {
			if raw != strings.TrimSpace(raw) || raw == "" {
				return Config{}, errors.New("invalid OIDC configuration")
			}
		}
		for _, raw := range []string{c.OIDCIssuer, c.OIDCJWKSURL} {
			u, err := url.Parse(raw)
			if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
				return Config{}, errors.New("invalid OIDC URL")
			}
		}
		seen := map[string]bool{}
		for _, id := range strings.Split(c.OIDCProviderClients, ",") {
			if id == "" || id != strings.TrimSpace(id) || id == c.OIDCInternalClient || seen[id] {
				return Config{}, errors.New("invalid OIDC provider mapping")
			}
			seen[id] = true
		}
	}
	return c, nil
}

var Module = fx.Module("config", fx.Provide(Load))
