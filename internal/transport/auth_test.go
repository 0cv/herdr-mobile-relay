package transport

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/config"
)

func TestTokenlessUpgradeAllowed(t *testing.T) {
	cfg := &config.Config{}
	req := httptest.NewRequest("GET", "/ws", nil)
	if !webSocketUpgradeAllowed(cfg, req) {
		t.Error("expected tokenless loopback upgrade to pass")
	}
}

func TestEncryptedUpgradeRequiresSubprotocol(t *testing.T) {
	cfg := &config.Config{Token: "secret123"}
	req := httptest.NewRequest("GET", "/ws", nil)
	if webSocketUpgradeAllowed(cfg, req) {
		t.Error("expected upgrade without encrypted subprotocol to fail")
	}
	req.Header.Set("Sec-WebSocket-Protocol", "chat, herdr-e2ee-v1")
	if !webSocketUpgradeAllowed(cfg, req) {
		t.Error("expected encrypted subprotocol to pass")
	}
}

func TestEncryptedUpgradeRejectsKeyInRequest(t *testing.T) {
	cfg := &config.Config{Token: "secret123"}
	for name, req := range map[string]*http.Request{
		"query":  httptest.NewRequest("GET", "/ws?token=secret123", nil),
		"header": httptest.NewRequest("GET", "/ws", nil),
	} {
		req.Header.Set("Sec-WebSocket-Protocol", e2eeSubprotocol)
		if name == "header" {
			req.Header.Set("Authorization", "Bearer secret123")
		}
		if webSocketUpgradeAllowed(cfg, req) {
			t.Errorf("%s credential unexpectedly accepted in HTTP request", name)
		}
	}
}

func TestEncryptedUpgradeAllowsAnyOriginAfterHandshakeGate(t *testing.T) {
	cfg := &config.Config{Token: "secret"}
	req := httptest.NewRequest("GET", "/ws", nil)
	req.Header.Set("Origin", "https://random-origin.com")
	req.Header.Set("Sec-WebSocket-Protocol", e2eeSubprotocol)
	if !webSocketUpgradeAllowed(cfg, req) {
		t.Error("expected authenticated encrypted handshake to gate arbitrary origin")
	}
}

func TestOriginRejectedWithoutToken(t *testing.T) {
	cfg := &config.Config{AllowedOrigins: []string{"https://allowed.com"}}
	req := httptest.NewRequest("GET", "/ws", nil)
	req.Header.Set("Origin", "https://evil.com")
	if webSocketUpgradeAllowed(cfg, req) {
		t.Error("expected origin to be rejected")
	}
}

func TestOriginAllowedExplicit(t *testing.T) {
	cfg := &config.Config{AllowedOrigins: []string{"https://allowed.com"}}
	req := httptest.NewRequest("GET", "/ws", nil)
	req.Header.Set("Origin", "https://allowed.com")
	if !webSocketUpgradeAllowed(cfg, req) {
		t.Error("expected explicit origin to be allowed")
	}
}

func TestOriginWildcard(t *testing.T) {
	cfg := &config.Config{AllowedOrigins: []string{"*"}}
	req := httptest.NewRequest("GET", "/ws", nil)
	req.Header.Set("Origin", "https://anything.com")
	if !webSocketUpgradeAllowed(cfg, req) {
		t.Error("expected wildcard origin to be allowed")
	}
}
