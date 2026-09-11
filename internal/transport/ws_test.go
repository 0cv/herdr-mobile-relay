package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/coder/websocket"
)

func TestDecodeWebSocketMessageRequiresUTF8JSONObject(t *testing.T) {
	valid, err := decodeWebSocketMessage([]byte(`{"type":"refresh_agents"}`))
	if err != nil {
		t.Fatal(err)
	}
	if valid["type"] != "refresh_agents" {
		t.Fatalf("decoded type = %v", valid["type"])
	}

	for _, test := range []struct {
		name string
		data []byte
	}{
		{name: "null", data: []byte(`null`)},
		{name: "array", data: []byte(`[]`)},
		{name: "malformed", data: []byte(`{`)},
		{name: "invalid UTF-8", data: []byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodeWebSocketMessage(test.data); err == nil {
				t.Fatal("invalid WebSocket message was accepted")
			}
		})
	}
}

func TestHubShutdownStopsOrderedIngress(t *testing.T) {
	hub := NewHub(&config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := hub.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-hub.ingressDone:
	default:
		t.Fatal("ordered ingress goroutine remained live after shutdown")
	}
	if err := hub.Shutdown(ctx); err != nil {
		t.Fatalf("second shutdown: %v", err)
	}
}

func handshakeDeadlineStacks(ctx context.Context, done <-chan struct{}) []byte {
	select {
	case <-ctx.Done():
	case <-done:
		if ctx.Err() == nil {
			return nil
		}
	}
	buffer := make([]byte, 256*1024)
	return buffer[:runtime.Stack(buffer, true)]
}

func cleanupHandshakeHub(hub *Hub, release func()) error {
	release()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- hub.Shutdown(ctx) }()
	select {
	case err := <-finished:
		return err
	case <-ctx.Done():
		return fmt.Errorf("handshake cleanup: %w", ctx.Err())
	}
}

func TestHandshakeDeadlineStacksBothReady(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	<-ctx.Done()
	done := make(chan struct{})
	close(done)
	dump := handshakeDeadlineStacks(ctx, done)
	if len(dump) == 0 || len(dump) > 256*1024 || !strings.Contains(string(dump), "TestHandshakeDeadlineStacksBothReady") {
		t.Fatalf("missing bounded deadline stack when completion is also ready: %d bytes", len(dump))
	}
	if dump := handshakeDeadlineStacks(context.Background(), done); dump != nil {
		t.Fatal("healthy completion collected stacks")
	}
}

func TestHandshakeEarlyFailureCleanup(t *testing.T) {
	for _, mode := range []string{"dial-failure", "missing-callback-notification"} {
		t.Run(mode, func(t *testing.T) {
			hub := NewHub(&config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			release := make(chan struct{})
			releaseCallback := sync.OnceFunc(func() { close(release) })
			callbackEntered := make(chan struct{})
			callbackDone := make(chan struct{})
			notification := make(chan struct{})
			hub.SetOnConnect(func(client *ClientConn) {
				close(callbackEntered)
				<-release
				close(callbackDone)
			})
			t.Cleanup(func() {
				if err := cleanupHandshakeHub(hub, releaseCallback); err != nil {
					t.Errorf("fallback cleanup: %v", err)
				}
			})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "dial-failure" {
					http.Error(w, "injected handshake failure", http.StatusServiceUnavailable)
					return
				}
				hub.HandleWebSocket(w, r)
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
			if mode == "dial-failure" {
				if err == nil {
					conn.CloseNow()
					t.Fatal("injected Dial failure succeeded")
				}
			} else {
				if err != nil {
					releaseCallback()
					t.Fatal(err)
				}
				defer conn.CloseNow()
				select {
				case <-callbackEntered:
				case <-ctx.Done():
					releaseCallback()
					t.Fatal("fault fixture callback did not enter")
				}
				waitCtx, stopWait := context.WithCancel(context.Background())
				stopWait()
				select {
				case <-notification:
					t.Fatal("fault fixture unexpectedly announced callback")
				case <-waitCtx.Done():
				}
			}
			if conn != nil {
				conn.CloseNow()
			}
			if err := cleanupHandshakeHub(hub, releaseCallback); err != nil {
				t.Fatal(err)
			}
			select {
			case <-hub.ingressDone:
			default:
				t.Fatal("early failure left ordered ingress running")
			}
			if mode == "missing-callback-notification" {
				select {
				case <-callbackDone:
				default:
					t.Fatal("early failure left callback running")
				}
			}
		})
	}
}

func TestOversizedHandshakeEvictsWithoutRegistrationDeadlock(t *testing.T) {
	hub := NewHub(&config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	started := time.Now()
	var phasesMu sync.Mutex
	phases := make(map[string]time.Duration)
	record := func(name string) {
		phasesMu.Lock()
		phases[name] = time.Since(started)
		phasesMu.Unlock()
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	releaseCallback := sync.OnceFunc(func() { close(release) })
	t.Cleanup(func() {
		if err := cleanupHandshakeHub(hub, releaseCallback); err != nil {
			t.Errorf("cleanup hub: %v", err)
		}
	})
	type sendResult struct {
		accepted bool
		canceled bool
	}
	result := make(chan sendResult, 1)
	hub.SetOnConnect(func(client *ClientConn) {
		record("callback entered")
		close(entered)
		<-release
		payload := strings.Repeat("x", clientOutboundMaxBytes+1)
		record("payload prepared")
		accepted := hub.Send(client, map[string]any{
			"type": "activity_history",
			"data": payload,
		})
		record("send returned")
		result <- sendResult{accepted: accepted, canceled: client.Context().Err() != nil}
	})
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local sockets unavailable: %v", err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(hub.HandleWebSocket))
	server.Listener = listener
	server.Start()
	defer server.Close()

	dialCtx, dialCancel := context.WithTimeout(context.Background(), time.Second)
	defer dialCancel()
	conn, _, err := websocket.Dial(dialCtx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	record("dial returned")
	if err != nil {
		releaseCallback()
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()
	select {
	case <-entered:
	case <-dialCtx.Done():
		releaseCallback()
		t.Fatal("callback was not entered")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	watchDone := make(chan struct{})
	stacks := make(chan []byte, 1)
	go func() { stacks <- handshakeDeadlineStacks(ctx, watchDone) }()
	record("shutdown starting")
	releaseCallback()
	shutdownErr := hub.Shutdown(ctx)
	record("shutdown returned")
	close(watchDone)
	dump := <-stacks
	if shutdownErr != nil {
		phasesMu.Lock()
		phaseSnapshot := make(map[string]time.Duration, len(phases))
		for name, elapsed := range phases {
			phaseSnapshot[name] = elapsed
		}
		phasesMu.Unlock()
		t.Fatalf("shutdown after oversized handshake: %v; phases=%v; deadline stacks (bounded):\n%s", shutdownErr, phaseSnapshot, dump)
	}
	select {
	case sent := <-result:
		if sent.accepted || !sent.canceled {
			t.Fatalf("oversized Send: accepted=%v canceled=%v", sent.accepted, sent.canceled)
		}
	default:
		t.Fatal("shutdown returned before oversized callback completed")
	}
	if got := hub.ClientCount(); got != 0 {
		t.Fatalf("connected clients = %d, want 0", got)
	}
}

func TestHubNegotiatesNoContextTakeoverCompression(t *testing.T) {
	hub := NewHub(&config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local sockets unavailable: %v", err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(hub.HandleWebSocket))
	server.Listener = listener
	server.Start()
	defer server.Close()

	conn, response, err := websocket.Dial(
		context.Background(),
		"ws"+strings.TrimPrefix(server.URL, "http"),
		&websocket.DialOptions{CompressionMode: websocket.CompressionNoContextTakeover},
	)
	if err != nil {
		t.Fatalf("dial compressed websocket: %v", err)
	}
	extension := response.Header.Get("Sec-WebSocket-Extensions")
	if !strings.Contains(extension, "permessage-deflate") ||
		!strings.Contains(extension, "client_no_context_takeover") ||
		!strings.Contains(extension, "server_no_context_takeover") {
		t.Fatalf("negotiated extensions = %q, want no-context permessage-deflate", extension)
	}
	if err := conn.Close(websocket.StatusNormalClosure, "done"); err != nil {
		t.Fatalf("close compressed websocket: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := hub.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown compressed hub: %v", err)
	}
}

func waitForHubClient(t *testing.T, hub *Hub) *ClientConn {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		hub.mu.RLock()
		for _, client := range hub.clients {
			hub.mu.RUnlock()
			return client
		}
		hub.mu.RUnlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatal("websocket client was not registered")
	return nil
}

func TestHubPreparedBatchCommitsAndDeliversInOrder(t *testing.T) {
	hub := NewHub(&config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
		t.Fatal(err)
	}
	defer conn.CloseNow()
	client := waitForHubClient(t, hub)
	committed := false
	if err := hub.BroadcastBatchPrepared(func() ([]any, func(), error) {
		return []any{
			map[string]any{"type": "inventory_status", "state": "ready"},
			map[string]any{"type": "agents", "agents": []any{}},
			map[string]any{"type": "workspaces", "workspaces": []any{}},
		}, func() { committed = true }, nil
	}); err != nil {
		t.Fatal(err)
	}
	if !committed {
		t.Fatal("prepared batch did not commit")
	}
	for index, want := range []string{"inventory_status", "agents", "workspaces"} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, data, readErr := conn.Read(ctx)
		cancel()
		if readErr != nil {
			t.Fatalf("read batch item %d: %v", index, readErr)
		}
		var message map[string]any
		if err := json.Unmarshal(data, &message); err != nil {
			t.Fatal(err)
		}
		if message["type"] != want {
			t.Fatalf("batch item %d = %#v, want type %q", index, message, want)
		}
	}
	if !hub.clientRegistered(client) {
		t.Fatal("client was not retained after prepared broadcast")
	}
	if !hub.SendBatchPrepared(client, func() []any {
		return []any{map[string]any{"type": "refresh", "state": "ready"}}
	}) {
		t.Fatal("prepared per-client send was rejected")
	}
	if !hub.SendBatchPreparedByID(client.ID(), func() []any {
		return []any{map[string]any{"type": "refresh", "state": "ready"}}
	}) {
		t.Fatal("prepared per-ID send was rejected")
	}
	for range 2 {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, data, readErr := conn.Read(ctx)
		cancel()
		if readErr != nil {
			t.Fatal(readErr)
		}
		var message map[string]any
		if err := json.Unmarshal(data, &message); err != nil || message["type"] != "refresh" {
			t.Fatalf("prepared per-client message = %s", data)
		}
	}
	conn.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := hub.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestHubPreparedBatchesKeepCommitAndDeliveryOrderAcrossConcurrentWriters(t *testing.T) {
	hub := NewHub(&config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
		t.Fatal(err)
	}
	defer conn.CloseNow()
	waitForHubClient(t, hub)
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	releaseFirstNow := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
	t.Cleanup(releaseFirstNow)
	secondEntered := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		results <- hub.BroadcastBatchPrepared(func() ([]any, func(), error) {
			close(firstEntered)
			<-releaseFirst
			return []any{map[string]any{"type": "batch", "id": "first"}}, func() {}, nil
		})
	}()
	select {
	case <-firstEntered:
	case <-time.After(time.Second):
		t.Fatal("first prepared batch did not enter")
	}
	go func() {
		results <- hub.BroadcastBatchPrepared(func() ([]any, func(), error) {
			close(secondEntered)
			return []any{map[string]any{"type": "batch", "id": "second"}}, func() {}, nil
		})
	}()
	select {
	case <-secondEntered:
		t.Fatal("second prepared batch overtook the registration barrier")
	case <-time.After(20 * time.Millisecond):
	}
	releaseFirstNow()
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	for index, want := range []string{"first", "second"} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, data, readErr := conn.Read(ctx)
		cancel()
		if readErr != nil {
			t.Fatalf("read concurrent batch %d: %v", index, readErr)
		}
		var message map[string]any
		if err := json.Unmarshal(data, &message); err != nil || message["id"] != want {
			t.Fatalf("concurrent batch %d = %s, want %q", index, data, want)
		}
	}
	conn.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := hub.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestHubPreparedBatchDoesNotCommitOnEncodingFailureAndHandlesEmptyRecipients(t *testing.T) {
	hub := NewHub(&config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := hub.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	}()
	commits := 0
	if err := hub.BroadcastBatchPrepared(func() ([]any, func(), error) {
		return nil, func() { commits++ }, nil
	}); err != nil {
		t.Fatal(err)
	}
	if commits != 1 {
		t.Fatalf("empty-recipient batch commits = %d, want 1", commits)
	}
	if err := hub.BroadcastBatchPrepared(func() ([]any, func(), error) {
		return []any{map[string]any{"bad": math.NaN()}}, func() { commits++ }, nil
	}); err == nil {
		t.Fatal("unsupported JSON value was accepted")
	}
	if commits != 1 {
		t.Fatalf("failed batch advanced commit count to %d", commits)
	}
}
