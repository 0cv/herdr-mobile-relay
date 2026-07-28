package transport

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/coder/websocket"
)

func TestHubDisconnectCallbackRunsOnce(t *testing.T) {
	hub := NewHub(&config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	disconnected := make(chan string, 2)
	hub.SetOnDisconnect(func(client *ClientConn) {
		disconnected <- client.ID()
	})

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local sockets unavailable: %v", err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(hub.HandleWebSocket))
	server.Listener = listener
	server.Start()
	defer server.Close()

	conn, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	if err := conn.Close(websocket.StatusNormalClosure, "done"); err != nil {
		t.Fatalf("close websocket: %v", err)
	}

	select {
	case clientID := <-disconnected:
		if clientID != "client-1" {
			t.Fatalf("disconnect client ID = %q, want client-1", clientID)
		}
	case <-time.After(time.Second):
		t.Fatal("disconnect callback did not run")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := hub.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	select {
	case clientID := <-disconnected:
		t.Fatalf("disconnect callback ran twice for %s", clientID)
	default:
	}
}
