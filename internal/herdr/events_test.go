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
		Agents: []SnapshotAgent{{
			PaneID: "pane-1",
			Agent:  "codex",
			Name:   "old title",
			Status: "working",
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

func TestSessionCacheDoesNotPromotePaneMetadataToRunningAgent(t *testing.T) {
	cache := NewSessionCache(SessionSnapshot{
		Panes: []SnapshotPane{{
			ID:          "pane-shell",
			TerminalID:  "term-shell",
			WorkspaceID: "workspace-1",
			TabID:       "tab-4",
			Agent:       "codex",
			Status:      "idle",
		}},
	})

	snapshot := cache.Snapshot()
	if len(snapshot.Panes) != 1 {
		t.Fatalf("panes = %#v, want one terminal pane", snapshot.Panes)
	}
	if snapshot.Panes[0].Agent != "" {
		t.Fatalf("pane agent = %q, want empty without an authoritative agent record", snapshot.Panes[0].Agent)
	}

	detected := "codex"
	changed, err := cache.Apply(Event{
		Event: "pane.agent_detected",
		Data:  json.RawMessage(`{"pane_id":"pane-shell","agent":"codex"}`),
	})
	if err != nil || !changed {
		t.Fatalf("agent detection changed=%v err=%v, want changed", changed, err)
	}
	if got := cache.Snapshot().Panes[0].Agent; got != detected {
		t.Fatalf("detected agent = %q, want %q", got, detected)
	}

	changed, err = cache.Apply(Event{
		Event: "pane.agent_detected",
		Data:  json.RawMessage(`{"pane_id":"pane-shell","released":true}`),
	})
	if err != nil || !changed {
		t.Fatalf("agent release changed=%v err=%v, want changed", changed, err)
	}
	if got := cache.Snapshot().Panes[0].Agent; got != "" {
		t.Fatalf("released agent = %q, want empty", got)
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

func TestSessionCacheAppliesDesktopTabOrder(t *testing.T) {
	cache := NewSessionCache(SessionSnapshot{
		Tabs: []Tab{
			{ID: "tab-1", WorkspaceID: "workspace-1", Label: "1", Number: 1},
			{ID: "tab-2", WorkspaceID: "workspace-1", Label: "second", Number: 2},
		},
	})
	// Captured from Herdr 0.8.0: numbers are stable identities and stay
	// unchanged; the ordered tabs array and refreshed auto-labels are the
	// only signals of the move.
	var event Event
	if err := json.Unmarshal([]byte(`{
		"event":"tab_moved",
		"data":{
			"type":"tab_moved",
			"tab_id":"tab-2",
			"workspace_id":"workspace-1",
			"insert_index":0,
			"tabs":[
				{"tab_id":"tab-2","workspace_id":"workspace-1","label":"second","number":2,"pane_count":1},
				{"tab_id":"tab-1","workspace_id":"workspace-1","label":"2","number":1,"pane_count":1}
			]
		}
	}`), &event); err != nil {
		t.Fatal(err)
	}
	changed, err := cache.Apply(event)
	if err != nil || !changed {
		t.Fatalf("Apply() changed=%v err=%v, want changed", changed, err)
	}
	tabs := cache.Snapshot().Tabs
	if len(tabs) != 2 || tabs[0].ID != "tab-2" || tabs[1].ID != "tab-1" {
		t.Fatalf("tabs = %+v, want desktop order tab-2 then tab-1", tabs)
	}
	if tabs[0].Number != 2 || tabs[1].Number != 1 || tabs[1].Label != "2" {
		t.Fatalf("tabs = %+v, want stable numbers and refreshed auto-label", tabs)
	}
}

func TestSessionCacheKeepsEmptyWorkspacesAndWorktreeChanges(t *testing.T) {
	cache := NewSessionCache(SessionSnapshot{
		Workspaces: []Workspace{
			{ID: "w1", Number: 1, Label: "Project"},
			{ID: "w2", Number: 2, Label: "Empty"},
		},
		Tabs: []Tab{{ID: "t2", WorkspaceID: "w2", Cwd: "/home/user/empty"}},
	})
	snapshot := cache.Snapshot()
	if len(snapshot.Workspaces) != 2 || snapshot.Workspaces[1].Cwd != "/home/user/empty" {
		t.Fatalf("initial workspaces = %+v", snapshot.Workspaces)
	}

	changed, err := cache.Apply(Event{
		Event: "worktree.opened",
		Data: json.RawMessage(`{
			"workspace":{
				"workspace_id":"w3",
				"number":3,
				"label":"fix/one",
				"worktree":{
					"repo_key":"repo",
					"repo_name":"project",
					"repo_root":"/home/user/project",
					"checkout_path":"/home/user/worktrees/fix",
					"is_linked_worktree":true
				}
			}
		}`),
	})
	if err != nil || !changed {
		t.Fatalf("Apply(worktree.opened) changed=%v err=%v", changed, err)
	}
	workspaces := cache.Snapshot().Workspaces
	if len(workspaces) != 3 || workspaces[2].Worktree == nil ||
		workspaces[2].Cwd != "/home/user/worktrees/fix" {
		t.Fatalf("workspaces after open = %+v", workspaces)
	}

	changed, err = cache.Apply(Event{
		Event: "workspace.renamed",
		Data:  json.RawMessage(`{"workspace_id":"w2","label":"Renamed"}`),
	})
	if err != nil || !changed || cache.Snapshot().Workspaces[1].Label != "Renamed" {
		t.Fatalf("rename changed=%v err=%v workspaces=%+v", changed, err, cache.Snapshot().Workspaces)
	}
}

func TestTopologySubscriptionsCoverWorkspaceAndWorktreeMutations(t *testing.T) {
	seen := make(map[string]bool)
	for _, subscription := range topologySubscriptions(true) {
		seen[subscription["type"]] = true
	}
	for _, event := range []string{
		"workspace.updated",
		"workspace.metadata_updated",
		"workspace.moved",
		"workspace.reordered",
		"worktree.created",
		"worktree.opened",
		"worktree.removed",
	} {
		if !seen[event] {
			t.Fatalf("topology subscription omits %s", event)
		}
	}
}

// Herdr 0.7.5 — the supported minimum — rejects the entire events.subscribe
// request when workspace.reordered appears in it, degrading realtime updates
// to polling. The name must be excluded unless the capability probe passes.
func TestTopologySubscriptionsGateWorkspaceReorderedBehindProbe(t *testing.T) {
	for _, subscription := range topologySubscriptions(false) {
		if subscription["type"] == "workspace.reordered" {
			t.Fatal("workspace.reordered subscribed without capability support")
		}
	}
	seen := make(map[string]bool)
	for _, subscription := range topologySubscriptions(false) {
		seen[subscription["type"]] = true
	}
	if !seen["workspace.moved"] || !seen["worktree.removed"] {
		t.Fatal("gating workspace.reordered dropped unrelated subscriptions")
	}
}

func TestEventSubscribeSendsGatedSubscriptionList(t *testing.T) {
	subscribedTypes := func(t *testing.T, probe func() bool) map[string]bool {
		t.Helper()
		socketPath := filepath.Join(t.TempDir(), "herdr.sock")
		listener, err := net.Listen("unix", socketPath)
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		defer listener.Close()
		received := make(chan []map[string]string, 1)
		go func() {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			defer conn.Close()
			var request struct {
				ID     string `json:"id"`
				Params struct {
					Subscriptions []map[string]string `json:"subscriptions"`
				} `json:"params"`
			}
			if json.NewDecoder(bufio.NewReader(conn)).Decode(&request) != nil {
				return
			}
			received <- request.Params.Subscriptions
			_ = writeTestJSON(conn, map[string]any{
				"id":     request.ID,
				"result": map[string]any{"type": "subscription_started"},
			})
		}()
		client := NewEventClient(socketPath)
		if probe != nil {
			client.SetWorkspaceReorderedProbe(probe)
		}
		stream, err := client.subscribe(context.Background())
		if err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		defer stream.Close()
		seen := make(map[string]bool)
		for _, subscription := range <-received {
			seen[subscription["type"]] = true
		}
		return seen
	}

	if seen := subscribedTypes(t, nil); seen["workspace.reordered"] {
		t.Fatal("default subscribe sent workspace.reordered without a probe")
	}
	if seen := subscribedTypes(t, func() bool { return true }); !seen["workspace.reordered"] {
		t.Fatal("supported build did not subscribe to workspace.reordered")
	}
}

// tab.created and tab.closed update only the tab and pane maps, so snapshots
// must derive the per-workspace counts instead of copying the stale values
// cached from earlier workspace events.
func TestSessionCacheSnapshotDerivesCountsFromTabAndPaneEvents(t *testing.T) {
	cache := NewSessionCache(SessionSnapshot{
		Workspaces: []Workspace{
			{ID: "w1", Number: 1, Label: "Project", TabCount: 1, PaneCount: 1, ActiveTabID: "t1"},
		},
		Tabs:  []Tab{{ID: "t1", WorkspaceID: "w1", Number: 1}},
		Panes: []SnapshotPane{{ID: "p1", TabID: "t1", WorkspaceID: "w1"}},
	})

	changed, err := cache.Apply(Event{
		Event: "tab.created",
		Data:  json.RawMessage(`{"tab":{"tab_id":"t2","workspace_id":"w1","number":2}}`),
	})
	if err != nil || !changed {
		t.Fatalf("Apply(tab.created) changed=%v err=%v", changed, err)
	}
	workspace := cache.Snapshot().Workspaces[0]
	if workspace.TabCount != 2 || workspace.PaneCount != 1 {
		t.Fatalf("workspace after tab.created = %+v, want tab_count=2 pane_count=1", workspace)
	}
	if workspace.ActiveTabID != "t1" {
		t.Fatalf("active_tab_id = %q, want authoritative t1", workspace.ActiveTabID)
	}

	changed, err = cache.Apply(Event{
		Event: "tab.closed",
		Data:  json.RawMessage(`{"tab_id":"t1"}`),
	})
	if err != nil || !changed {
		t.Fatalf("Apply(tab.closed) changed=%v err=%v", changed, err)
	}
	workspace = cache.Snapshot().Workspaces[0]
	if workspace.TabCount != 1 || workspace.PaneCount != 0 {
		t.Fatalf("workspace after tab.closed = %+v, want tab_count=1 pane_count=0", workspace)
	}
	if workspace.ActiveTabID == "t1" {
		t.Fatal("closed tab t1 is still reported active")
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
