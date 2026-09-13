package app

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
	"github.com/coder/websocket"
)

// Real Unix-socket inventory operations; no State mutation or substitute
// publisher. The atomic outcome controls only the fake Herdr process boundary.
func inventoryBarrierFixture(t *testing.T, ctx context.Context) (*Server, *atomic.Int32) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "herdr.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	outcome := &atomic.Int32{}
	outcome.Store(1)
	var lastReady atomic.Int32
	lastReady.Store(1)
	var wg sync.WaitGroup
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
				var request struct {
					ID     string `json:"id"`
					Method string `json:"method"`
				}
				if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&request); err != nil {
					return
				}
				gen := outcome.Load()
				if gen < 1 {
					gen = lastReady.Load()
				} else {
					lastReady.Store(gen)
				}
				workspace := map[string]any{"workspace_id": "workspace-1", "label": fmt.Sprintf("Workspace v%d", gen), "cwd": fmt.Sprintf("/work/v%d", gen), "pane_count": 1}
				pane := map[string]any{"pane_id": "pane-1", "terminal_id": "terminal-1", "workspace_id": "workspace-1", "tab_id": "tab-1", "name": fmt.Sprintf("agent-v%d", gen), "agent": "codex", "agent_status": "idle", "cwd": fmt.Sprintf("/work/v%d", gen)}
				var result any
				switch request.Method {
				case "agent.list":
					if outcome.Load() < 0 {
						_ = json.NewEncoder(conn).Encode(map[string]any{"id": request.ID, "error": map[string]any{"code": "server_not_running", "message": "fixture unavailable"}})
						return
					}
					result = map[string]any{"type": "agent_list", "agents": []any{pane}}
				case "workspace.list":
					result = map[string]any{"type": "workspace_list", "workspaces": []any{workspace}}
				case "pane.list", "tab.list":
					_ = json.NewEncoder(conn).Encode(map[string]any{"id": request.ID, "error": map[string]any{"code": "unsupported", "message": "fixture"}})
					return
				case "events.subscribe":
					_ = json.NewEncoder(conn).Encode(map[string]any{"id": request.ID, "result": map[string]any{"type": "subscription_started"}})
					<-ctx.Done()
					return
				case "session.snapshot":
					result = map[string]any{"type": "session_snapshot", "snapshot": map[string]any{"workspaces": []any{workspace}, "panes": []any{pane}, "agents": []any{pane}}}
				default:
					t.Errorf("unexpected method %s", request.Method)
					return
				}
				_ = json.NewEncoder(conn).Encode(map[string]any{"id": request.ID, "result": result})
			}()
		}
	}()
	t.Cleanup(func() { _ = listener.Close(); <-done; wg.Wait() })
	server := New(&config.Config{Host: "127.0.0.1", Port: 8375, SocketPath: path, PollInterval: 3600, CacheDir: t.TempDir(), ConfigHome: t.TempDir(), RuntimeDir: t.TempDir()}, "0.9.0", "fixture", slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() {
		_ = server.conversationB.Close()
		shutdown, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.hub.Shutdown(shutdown)
	})
	server.setInventoryPublisher(ctx)
	return server, outcome
}

func TestProductionInventoryOutcomesAtPublicationBarriers(t *testing.T) {
	for _, source := range []string{"poll", "event"} {
		for _, action := range []string{"registration", "immediate", "deferred"} {
			for _, order := range []string{"send-first", "recovery-first"} {
				t.Run(source+"/"+action+"/"+order, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
					defer cancel()
					server, outcome := inventoryBarrierFixture(t, ctx)
					var phase atomic.Value
					phase.Store("")
					entered, release := make(chan struct{}), make(chan struct{})
					var once, releaseOnce sync.Once
					defer releaseOnce.Do(func() { close(release) })
					var holdRecovery atomic.Bool
					allowRecovery := make(chan struct{})
					var allowRecoveryOnce sync.Once
					defer allowRecoveryOnce.Do(func() { close(allowRecovery) })
					server.inventoryPublicationObserver = func(at string) {
						if at == "publication" && holdRecovery.Load() {
							select {
							case <-allowRecovery:
							case <-ctx.Done():
							}
						}
						if at == phase.Load().(string) {
							once.Do(func() {
								close(entered)
								select {
								case <-release:
								case <-ctx.Done():
								}
							})
						}
					}
					pollDone := make(chan struct{})
					go func() { defer close(pollDone); server.poller.Run(ctx) }()
					defer func() { cancel(); <-pollDone }()
					wait := func(predicate func() bool) {
						t.Helper()
						for !predicate() {
							select {
							case <-ctx.Done():
								t.Fatal("inventory barrier timed out")
							case <-time.After(time.Millisecond):
							}
						}
					}
					wait(func() bool { return server.committedInventoryStatus()["state"] == "ready" })
					outcome.Store(-1)
					server.poller.Wake()
					wait(func() bool { return server.committedInventoryStatus()["state"] == "error" })
					connected := make(chan *transport.ClientConn, 4)
					server.hub.SetOnConnect(func(client *transport.ClientConn) { server.sendConnectionSnapshot(client); connected <- client })
					httpServer := httptest.NewServer(http.HandlerFunc(server.hub.HandleWebSocket))
					defer httpServer.Close()
					dial := func() *websocket.Conn {
						t.Helper()
						conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(httpServer.URL, "http"), nil)
						if err != nil {
							t.Fatal(err)
						}
						return conn
					}
					read := func(conn *websocket.Conn) map[string]any {
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
					assertStatus := func(status map[string]any, want string) {
						t.Helper()
						code := ""
						if want == "error" {
							code = "server_not_running"
						}
						if status["state"] != want || status["error_code"] != code || status["stale"] != (want == "error") {
							t.Fatalf("incoherent status=%#v want=%s", status, want)
						}
						if want == "error" && status["message"] == "" {
							t.Fatal("error without message")
						}
					}
					assertRows := func(message map[string]any, gen int) {
						t.Helper()
						switch message["type"] {
						case "agents":
							rows := message["agents"].([]any)
							if len(rows) != 1 {
								t.Fatalf("agents=%#v", message)
							}
							row := rows[0].(map[string]any)
							if row["pane_id"] != "pane-1" || row["workspace_id"] != "workspace-1" || row["name"] != fmt.Sprintf("agent-v%d", gen) || row["cwd"] != fmt.Sprintf("/work/v%d", gen) || row["status"] != "idle" {
								t.Fatalf("incoherent agents gen=%d: %#v", gen, message)
							}
						case "workspaces":
							rows := message["workspaces"].([]any)
							if len(rows) != 1 {
								t.Fatalf("workspaces=%#v", message)
							}
							row := rows[0].(map[string]any)
							if row["workspace_id"] != "workspace-1" || row["label"] != fmt.Sprintf("Workspace v%d", gen) || row["cwd"] != fmt.Sprintf("/work/v%d", gen) {
								t.Fatalf("incoherent workspaces gen=%d: %#v", gen, message)
							}
						}
					}
					handshake := func(conn *websocket.Conn, want string, gen int) {
						t.Helper()
						for index, kind := range []string{"push_config", "agents", "workspaces", "activity_history", "inventory_status"} {
							message := read(conn)
							if message["type"] != kind {
								t.Fatalf("handshake %d=%#v", index, message)
							}
							if kind == "push_config" {
								assertStatus(message["inventory"].(map[string]any), want)
							} else if kind == "inventory_status" {
								assertStatus(message, want)
							} else {
								assertRows(message, gen)
							}
						}
					}
					batch := func(conn *websocket.Conn, want string, gen int) {
						t.Helper()
						for _, kind := range []string{"inventory_status", "agents", "workspaces"} {
							message := read(conn)
							if message["type"] != kind {
								t.Fatalf("batch=%#v want=%s", message, kind)
							}
							if kind == "inventory_status" {
								assertStatus(message, want)
							} else {
								assertRows(message, gen)
							}
						}
					}
					var conn *websocket.Conn
					var client *transport.ClientConn
					if action != "registration" {
						conn = dial()
						defer conn.CloseNow()
						client = <-connected
						handshake(conn, "error", 1)
						wait(func() bool { return server.hub.ClientCount() == 1 })
					}
					if order == "send-first" {
						// Flush the handshake before recovery enqueues its adjacent
						// inventory_status; the transport intentionally coalesces
						// consecutive same-type snapshots, not complete tuples.
						holdRecovery.Store(action == "registration")
						phase.Store(action)
					} else {
						phase.Store("publication")
					}
					recoveryDone := make(chan struct{})
					recoverInventory := func() {
						outcome.Store(2)
						if source == "poll" {
							server.poller.Wake()
							close(recoveryDone)
						} else {
							go func() {
								defer close(recoveryDone)
								server.poller.SetEventReconnectWait(func(context.Context) bool { return false })
								server.poller.RunEvents(ctx, herdr.NewEventClient(server.cfg.SocketPath))
							}()
						}
					}
					actionDone := make(chan struct{})
					send := func() {
						switch action {
						case "registration":
							conn = dial()
							go func() { client = <-connected; close(actionDone) }()
						case "immediate":
							go func() { defer close(actionDone); server.requestAgentRefresh(client) }()
						case "deferred":
							server.refreshMu.Lock()
							server.refreshClients[client.ID()] = true
							server.refreshMu.Unlock()
							go func() { defer close(actionDone); server.sendRequestedAgentRefreshes() }()
						}
					}
					if order == "send-first" {
						send()
						select {
						case <-entered:
						case <-ctx.Done():
							t.Fatal("send barrier not reached")
						}
						recoverInventory()
						wait(func() bool { return server.state.InventorySnapshot().Status["state"] == "ready" })
					} else {
						recoverInventory()
						select {
						case <-entered:
						case <-ctx.Done():
							t.Fatal("recovery publication barrier not reached")
						}
						send()
					}
					select {
					case <-actionDone:
						t.Fatal("send escaped publication barrier")
					default:
					}
					releaseOnce.Do(func() { close(release) })
					select {
					case <-actionDone:
					case <-ctx.Done():
						t.Fatal("send did not complete")
					}
					defer conn.CloseNow()
					if action == "registration" {
						if order == "send-first" {
							handshake(conn, "error", 1)
							allowRecoveryOnce.Do(func() { close(allowRecovery) })
							batch(conn, "ready", 2)
						} else {
							handshake(conn, "ready", 2)
						}
					} else {
						if order == "send-first" {
							batch(conn, "error", 1)
						}
						batch(conn, "ready", 2)
						if order == "recovery-first" {
							batch(conn, "ready", 2)
						}
						if action == "immediate" {
							batch(conn, "ready", 2)
						}
					}
					wait(func() bool { return server.committedInventoryStatus()["state"] == "ready" })
					// Connected unchanged-success completion must drain an explicit refresh.
					server.requestAgentRefresh(client)
					batch(conn, "ready", 2)
					batch(conn, "ready", 2)
					server.refreshMu.Lock()
					pending := len(server.refreshClients)
					server.refreshMu.Unlock()
					if pending != 0 {
						t.Fatal("unchanged success left refresh pending")
					}
					conn.CloseNow()
					wait(func() bool { return server.hub.ClientCount() == 0 })
					// Real failure and recovery with zero listeners, followed by reconnect.
					outcome.Store(-1)
					server.poller.Wake()
					wait(func() bool { return server.committedInventoryStatus()["state"] == "error" })
					reconnect := dial()
					defer reconnect.CloseNow()
					client = <-connected
					handshake(reconnect, "error", 2)
					wait(func() bool { return server.hub.ClientCount() == 1 })
					server.requestAgentRefresh(client)
					batch(reconnect, "error", 2)
					batch(reconnect, "error", 2)
					reconnect.CloseNow()
					wait(func() bool { return server.hub.ClientCount() == 0 })
					outcome.Store(2)
					server.poller.Wake()
					wait(func() bool { return server.committedInventoryStatus()["state"] == "ready" })
					final := dial()
					defer final.CloseNow()
					<-connected
					handshake(final, "ready", 2)
					cancel()
					<-recoveryDone
					t.Log("real outcomes crossed barrier in required order; coherent status/error/stale and agent/workspace contents; unchanged/repeated refresh, zero listeners and reconnect passed")
				})
			}
		}
	}
}
