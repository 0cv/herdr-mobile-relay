package transport

import (
	"net/http/httptest"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/config"
)

func TestNoTokenRequired(t *testing.T) {
	cfg := &config.Config{Token: ""}
	req := httptest.NewRequest("GET", "/ws", nil)
	if !Authenticate(cfg, req) {
		t.Error("expected auth to pass with no token configured")
	}
}

func TestTokenViaBearerHeader(t *testing.T) {
	cfg := &config.Config{Token: "secret123"}
	req := httptest.NewRequest("GET", "/ws", nil)
	req.Header.Set("Authorization", "Bearer secret123")
	if !Authenticate(cfg, req) {
		t.Error("expected auth to pass with correct bearer token")
	}
}

func TestTokenViaQueryParam(t *testing.T) {
	cfg := &config.Config{Token: "secret123"}
	req := httptest.NewRequest("GET", "/ws?token=secret123", nil)
	if !Authenticate(cfg, req) {
		t.Error("expected auth to pass with correct query token")
	}
}

func TestWrongTokenRejected(t *testing.T) {
	cfg := &config.Config{Token: "secret123"}
	req := httptest.NewRequest("GET", "/ws", nil)
	req.Header.Set("Authorization", "Bearer wrongtoken")
	if Authenticate(cfg, req) {
		t.Error("expected auth to fail with wrong token")
	}
}

func TestMissingTokenRejected(t *testing.T) {
	cfg := &config.Config{Token: "secret123"}
	req := httptest.NewRequest("GET", "/ws", nil)
	if Authenticate(cfg, req) {
		t.Error("expected auth to fail with missing token")
	}
}

func TestOriginAllowedNoOriginHeader(t *testing.T) {
	cfg := &config.Config{Token: "secret"}
	req := httptest.NewRequest("GET", "/ws", nil)
	req.Header.Set("Authorization", "Bearer secret")
	if !Authenticate(cfg, req) {
		t.Error("expected auth to pass with no origin header")
	}
}

func TestOriginAllowedWithToken(t *testing.T) {
	cfg := &config.Config{Token: "secret"}
	req := httptest.NewRequest("GET", "/ws", nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Origin", "https://random-origin.com")
	if !Authenticate(cfg, req) {
		t.Error("expected any origin to be allowed when token is configured")
	}
}

func TestOriginRejectedWithoutToken(t *testing.T) {
	cfg := &config.Config{Token: "", AllowedOrigins: []string{"https://allowed.com"}}
	req := httptest.NewRequest("GET", "/ws", nil)
	req.Header.Set("Origin", "https://evil.com")
	if Authenticate(cfg, req) {
		t.Error("expected origin to be rejected")
	}
}

func TestOriginAllowedExplicit(t *testing.T) {
	cfg := &config.Config{Token: "", AllowedOrigins: []string{"https://allowed.com"}}
	req := httptest.NewRequest("GET", "/ws", nil)
	req.Header.Set("Origin", "https://allowed.com")
	if !Authenticate(cfg, req) {
		t.Error("expected explicit origin to be allowed")
	}
}

func TestOriginWildcard(t *testing.T) {
	cfg := &config.Config{Token: "", AllowedOrigins: []string{"*"}}
	req := httptest.NewRequest("GET", "/ws", nil)
	req.Header.Set("Origin", "https://anything.com")
	if !Authenticate(cfg, req) {
		t.Error("expected wildcard origin to be allowed")
	}
}
