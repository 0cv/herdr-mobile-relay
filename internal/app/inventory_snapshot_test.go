package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
	"github.com/coder/websocket"
)

func snapshotTestClient(t *testing.T, ctx context.Context, server *Server) (*transport.ClientConn, *websocket.Conn) {
	t.Helper()
	connected := make(chan *transport.ClientConn, 1)
	server.hub.SetOnConnect(func(client *transport.ClientConn) { connected <- client })
	httpServer := httptest.NewServer(http.HandlerFunc(server.hub.HandleWebSocket))
	t.Cleanup(httpServer.Close)
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(httpServer.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	select {
	case client := <-connected:
		return client, conn
	case <-ctx.Done():
		t.Fatal("client registration timed out")
		return nil, nil
	}
}

func readInventorySnapshotTestMessage(t *testing.T, ctx context.Context, conn *websocket.Conn) map[string]any {
	t.Helper()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var message map[string]any
	if err := json.Unmarshal(data, &message); err != nil {
		t.Fatal(err)
	}
	return message
}

func TestInventorySnapshotLegacyRefreshCompatibility(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server, _ := inventoryBarrierFixture(t, ctx)
	client, conn := snapshotTestClient(t, ctx, server)
	server.requestAgentRefresh(client)
	for _, kind := range []string{"inventory_status", "agents", "workspaces"} {
		message := readInventorySnapshotTestMessage(t, ctx, conn)
		if message["type"] != kind || message["snapshot_request_id"] != nil {
			t.Fatalf("legacy client behavior changed: %#v", message)
		}
	}
	server.hub.Send(client, map[string]any{"type": "marker"})
	if message := readInventorySnapshotTestMessage(t, ctx, conn); message["type"] != "marker" {
		t.Fatalf("legacy request emitted a correlated response: %#v", message)
	}
}

func TestInventorySnapshotNonceBounds(t *testing.T) {
	for _, nonce := range []string{"", "short", strings.Repeat("A", 23), strings.Repeat("!", 22), strings.Repeat("A", 21) + "B"} {
		if validInventorySnapshotNonce(nonce) {
			t.Fatalf("accepted invalid nonce %q", nonce)
		}
	}
	if !validInventorySnapshotNonce(strings.Repeat("A", 22)) {
		t.Fatal("rejected canonical 128-bit nonce")
	}
}

func TestInventorySnapshotOnlyNextStartedPollAnswers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	server, _ := inventoryBarrierFixture(t, ctx)
	client, conn := snapshotTestClient(t, ctx, server)
	started := make(chan int32, 2)
	release := make(chan struct{}, 2)
	var starts atomic.Int32
	server.poller.SetOnPollStart(func() func([]*coordinator.AgentState, []herdr.Workspace, bool) {
		complete := server.beginRequestedInventorySnapshots()
		started <- starts.Add(1)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return complete
	})
	done := make(chan struct{})
	go func() { defer close(done); server.poller.Run(ctx) }()
	defer func() { cancel(); <-done }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("first poll did not start")
	}
	nonce := strings.Repeat("A", 22)
	server.requestInventorySnapshot(client, nonce)
	release <- struct{}{}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("post-request poll did not start")
	}
	// The first poll's legacy publication may render data, but cannot carry the
	// newly admitted nonce. The second poll is held before querying Herdr.
	for _, kind := range []string{"inventory_status", "agents", "workspaces"} {
		message := readInventorySnapshotTestMessage(t, ctx, conn)
		if message["type"] != kind || message["snapshot_request_id"] != nil {
			t.Fatalf("in-flight poll answered new request: %#v", message)
		}
	}
	release <- struct{}{}
	message := readInventorySnapshotTestMessage(t, ctx, conn)
	if message["type"] != "inventory_snapshot" || message["snapshot_request_id"] != nonce {
		t.Fatalf("missing correlated response: %#v", message)
	}
	status := message["inventory"].(map[string]any)
	if status["state"] != "ready" || status["stale"] != false {
		t.Fatalf("unexpected status: %#v", status)
	}
}

func TestInventorySnapshotLatestWinsFailureAndExpiry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server, _ := inventoryBarrierFixture(t, ctx)
	client, conn := snapshotTestClient(t, ctx, server)
	server.requestInventorySnapshot(client, strings.Repeat("A", 22))
	latest := strings.Repeat("B", 21) + "A"
	server.requestInventorySnapshot(client, latest)
	server.refreshMu.Lock()
	count := len(server.snapshotRequests)
	server.refreshMu.Unlock()
	if count != 1 {
		t.Fatalf("pending count=%d, want one", count)
	}
	complete := server.beginRequestedInventorySnapshots()
	complete([]*coordinator.AgentState{{PaneID: "never-publish", Name: "retained-secret"}}, nil, false)
	message := readInventorySnapshotTestMessage(t, ctx, conn)
	if message["snapshot_request_id"] != latest || len(message["agents"].([]any)) != 0 || message["inventory"].(map[string]any)["state"] != "error" {
		t.Fatalf("failed poll exposed retained rows or wrong nonce: %#v", message)
	}
	server.refreshMu.Lock()
	server.snapshotRequests = map[string]inventorySnapshotRequest{client.ID(): {nonce: latest, expiresAt: time.Now().Add(-time.Second)}}
	server.refreshMu.Unlock()
	complete = server.beginRequestedInventorySnapshots()
	complete(nil, nil, true)
	// A later live marker must be next: an expired request emitted nothing.
	server.hub.Send(client, map[string]any{"type": "marker"})
	if message := readInventorySnapshotTestMessage(t, ctx, conn); message["type"] != "marker" {
		t.Fatalf("expired response was emitted: %#v", message)
	}
}
