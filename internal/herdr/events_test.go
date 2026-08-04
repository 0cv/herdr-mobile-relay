package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"testing"
)

func TestEventClientBootstrapsWithBufferedEvents(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "herdr.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	serverErr := make(chan error, 1)
	go func() {
		for range 2 {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				serverErr <- acceptErr
				return
			}
			decoder := json.NewDecoder(bufio.NewReader(conn))
			var request struct {
				ID     string `json:"id"`
				Method string `json:"method"`
			}
			if decodeErr := decoder.Decode(&request); decodeErr != nil {
				_ = conn.Close()
				serverErr <- decodeErr
				return
			}
			switch request.Method {
			case "events.subscribe":
				if err := writeTestJSON(conn, map[string]any{
					"id":     request.ID,
					"result": map[string]any{"type": "subscription_started"},
				}); err != nil {
					_ = conn.Close()
					serverErr <- err
					return
				}
				if err := writeTestJSON(conn, map[string]any{
					"event": "pane_closed",
					"data":  map[string]any{"type": "pane_closed", "pane_id": "pane-1", "workspace_id": "workspace-1"},
				}); err != nil {
					_ = conn.Close()
					serverErr <- err
					return
				}
			case "session.snapshot":
				if err := writeTestJSON(conn, map[string]any{
					"id": request.ID,
					"result": map[string]any{
						"type": "session_snapshot",
						"snapshot": map[string]any{
							"version":  "0.8.0",
							"protocol": 19,
							"tabs": []any{
								map[string]any{"tab_id": "tab-1", "workspace_id": "workspace-1", "number": 1, "label": "main"},
							},
							"panes": []any{
								map[string]any{"pane_id": "pane-1", "terminal_id": "term-1", "workspace_id": "workspace-1", "tab_id": "tab-1", "agent_status": "working", "revision": 1},
							},
							"agents": []any{
								map[string]any{"pane_id": "pane-1", "terminal_id": "term-1", "workspace_id": "workspace-1", "tab_id": "tab-1", "agent": "codex", "agent_status": "working", "name": "project", "revision": 1, "state_change_seq": 2},
							},
						},
					},
				}); err != nil {
					_ = conn.Close()
					serverErr <- err
					return
				}
			default:
				_ = conn.Close()
				serverErr <- fmt.Errorf("unexpected method %q", request.Method)
				return
			}
			_ = conn.Close()
		}
		serverErr <- nil
	}()

	client := NewEventClient(socketPath)
	stream, snapshot, buffered, err := client.Bootstrap(context.Background())
	if err != nil {
		t.Fatalf("Bootstrap() error = %v", err)
	}
	defer stream.Close()
	if snapshot.Protocol != 19 || len(snapshot.Agents) != 1 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if len(buffered) != 1 || buffered[0].Event != "pane.closed" {
		t.Fatalf("buffered events = %#v", buffered)
	}
	cache := NewSessionCache(snapshot)
	changed, err := cache.Apply(buffered[0])
	if err != nil || !changed {
		t.Fatalf("Apply() changed=%v err=%v", changed, err)
	}
	if got := len(cache.Snapshot().Panes); got != 0 {
		t.Fatalf("cached panes = %d, want 0 after pane.closed", got)
	}
	if serverErr := <-serverErr; serverErr != nil {
		t.Fatal(serverErr)
	}
}

func TestSessionCacheCoalescesTerminalLocalPaneUpdates(t *testing.T) {
	cache := NewSessionCache(SessionSnapshot{
		Panes: []SnapshotPane{{
			ID:          "pane-1",
			TerminalID:  "term-1",
			WorkspaceID: "workspace-1",
			TabID:       "tab-1",
			Label:       "old title",
			Agent:       "codex",
			Status:      "working",
			Revision:    5,
		}},
	})

	changed, err := cache.Apply(Event{
		Event: "pane.updated",
		Data:  json.RawMessage(`{"pane":{"pane_id":"pane-1","terminal_id":"term-1","workspace_id":"workspace-1","tab_id":"tab-1","label":"new title","agent":"codex","agent_status":"working","revision":6,"scroll":{"max_offset_from_bottom":12}}}`),
	})
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if changed {
		t.Fatal("terminal-local pane update triggered a topology commit")
	}
	pane := cache.Snapshot().Panes[0]
	if pane.Name != "new title" || pane.Revision != 6 || pane.Scroll.MaxOffsetFromBottom != 12 {
		t.Fatalf("cached pane = %+v, want updated local metadata", pane)
	}

	changed, err = cache.Apply(Event{
		Event: "pane.updated",
		Data:  json.RawMessage(`{"pane":{"pane_id":"pane-1","terminal_id":"term-1","workspace_id":"workspace-2","tab_id":"tab-1","label":"new title","agent":"codex","agent_status":"working","revision":7}}`),
	})
	if err != nil {
		t.Fatalf("Apply() topology error = %v", err)
	}
	if !changed {
		t.Fatal("workspace move did not trigger a topology commit")
	}
}

func TestSessionCacheIgnoresStalePaneUpdates(t *testing.T) {
	cache := NewSessionCache(SessionSnapshot{
		Panes: []SnapshotPane{{
			ID:       "pane-1",
			Revision: 5,
			Status:   "working",
		}},
	})
	changed, err := cache.Apply(Event{
		Event: "pane.updated",
		Data:  json.RawMessage(`{"pane":{"pane_id":"pane-1","revision":4,"agent_status":"idle"}}`),
	})
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if changed {
		t.Fatal("stale pane update was applied")
	}
	pane := cache.Snapshot().Panes[0]
	if pane.Revision != 5 || pane.Status != "working" {
		t.Fatalf("cached pane = %+v, want revision 5 working", pane)
	}
}

func TestSessionCacheUpdatesMovedTabMetadata(t *testing.T) {
	cache := NewSessionCache(SessionSnapshot{
		Tabs: []Tab{{
			ID:          "tab-1",
			WorkspaceID: "workspace-1",
			Label:       "main",
			Number:      1,
		}},
	})
	changed, err := cache.Apply(Event{
		Event: "tab.moved",
		Data:  json.RawMessage(`{"tab_id":"tab-1","workspace_id":"workspace-2","number":3}`),
	})
	if err != nil || !changed {
		t.Fatalf("Apply() changed=%v err=%v, want changed", changed, err)
	}
	snapshot := cache.Snapshot()
	if len(snapshot.Tabs) != 1 {
		t.Fatalf("tabs = %#v, want one tab", snapshot.Tabs)
	}
	tab := snapshot.Tabs[0]
	if tab.WorkspaceID != "workspace-2" || tab.Number != 3 || tab.Label != "main" {
		t.Fatalf("moved tab = %+v, want updated workspace/number and preserved label", tab)
	}
}

func writeTestJSON(conn net.Conn, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	_, err = conn.Write(payload)
	return err
}
