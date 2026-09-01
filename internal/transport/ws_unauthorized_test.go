package transport

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/0cv/herdr-mobile-relay/internal/config"
)

// rejectingResolver stands in for a relay whose device store no longer knows
// the credential a phone keeps presenting.
type rejectingResolver struct{}

func (rejectingResolver) ResolveE2EESecret(context.Context, E2EEAuthSelector) ([]byte, error) {
	return nil, errors.New("unknown credential")
}

func (rejectingResolver) CompleteE2EEAuth(context.Context, E2EEAuthSelector, bool) (E2EEAuthResult, error) {
	return E2EEAuthResult{}, errors.New("unknown credential")
}

// A phone whose pairing is gone would otherwise reconnect with the same dead
// credential forever, so the relay has to say the refusal is permanent.
func TestRejectedHandshakeClosesWithUnauthorizedCode(t *testing.T) {
	hub := NewHub(&config.Config{Token: "relay-token"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	hub.SetE2EEAuthResolver(rejectingResolver{})
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local sockets unavailable: %v", err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(hub.HandleWebSocket))
	server.Listener = listener
	server.Start()
	defer server.Close()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = hub.Shutdown(shutdownCtx)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), &websocket.DialOptions{
		Subprotocols: []string{e2eeSubprotocol},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()
	clientKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hello, err := json.Marshal(map[string]any{
		"type":         "e2ee_client_hello",
		"version":      e2eeVersion,
		"auth_kind":    "credential",
		"auth_id":      "credential-that-no-longer-exists",
		"auth_version": 1,
		"nonce":        base64.RawURLEncoding.EncodeToString(make([]byte, e2eeNonceBytes)),
		"public_key":   base64.RawURLEncoding.EncodeToString(clientKey.PublicKey().Bytes()),
		"proof":        base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageText, hello); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	if _, _, err = conn.Read(ctx); err == nil {
		t.Fatal("relay answered a rejected handshake")
	}
	if status := websocket.CloseStatus(err); status != websocket.StatusCode(UnauthorizedCloseCode) {
		t.Fatalf("close status = %d, want %d", status, UnauthorizedCloseCode)
	}
}
