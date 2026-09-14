package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEventClientBootstrapsWithBufferedEvents(t *testing.T) {
	releaseLive := make(chan struct{})
	var releaseLiveOnce sync.Once
	releaseLiveNow := func() { releaseLiveOnce.Do(func() { close(releaseLive) }) }
	t.Cleanup(releaseLiveNow)
	eventConn := newControlledEventConn(
		testJSONBatch(t,
			map[string]any{
				"id":     eventSubscriptionRequestID,
				"result": map[string]any{"type": "subscription_started"},
			},
			map[string]any{
				"event": "pane_closed",
				"data":  map[string]any{"type": "pane_closed", "pane_id": "pane-1", "workspace_id": "workspace-1"},
			},
		),
		testJSONBatch(t, map[string]any{
			"event": "pane_created",
			"data":  map[string]any{"type": "pane_created", "pane_id": "pane-live"},
		}),
		releaseLive,
	)
	snapshotConn := newControlledEventConn(testJSONBatch(t, map[string]any{
		"id": "mobile-relay-snapshot",
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
	}), nil, nil)
	client := newControlledEventClient(eventConn, snapshotConn)
	stream, snapshot, buffered, err := client.Bootstrap(context.Background())
	if err != nil {
		t.Fatalf("Bootstrap() error = %v", err)
	}
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
	releaseLiveNow()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	live, err := stream.Next(ctx)
	cancel()
	if err != nil || live.Event != "pane.created" {
		t.Fatalf("live event = %#v, err=%v, want one pane.created", live, err)
	}
	select {
	case <-eventConn.eofRead:
	case <-time.After(time.Second):
		t.Fatal("event reader did not observe EOF")
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	_, err = stream.Next(ctx)
	cancel()
	if err == nil {
		t.Fatal("live event was delivered more than once")
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close stream: %v", err)
	}
}

func TestEventStreamPrefetchesBufferedLinesAndReadsLaterEvents(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	reader := bufio.NewReader(client)
	stream := &EventStream{conn: client, queue: newEventQueue()}
	initial := []byte(
		`{"id":"mobile-relay-events","result":{"type":"subscription_started"}}` + "\n" +
			`{"event":"pane_closed","data":{"pane_id":"buffered"}}` + "\n" +
			`{"event":"pane_created","data":{"pane_id":"live"`,
	)
	writeDone := make(chan error, 1)
	go func() {
		_, err := server.Write(initial)
		writeDone <- err
	}()
	if _, err := readSocketAPILine(reader); err != nil {
		t.Fatalf("read subscription response: %v", err)
	}
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("write preloaded stream: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("preloaded stream write did not finish")
	}

	if !stream.readBuffered(reader) {
		t.Fatal("buffered event read failed")
	}
	buffered := stream.drain()
	if len(buffered) != 1 || buffered[0].Event != "pane.closed" {
		t.Fatalf("prefetched events = %#v", buffered)
	}
	if reader.Buffered() == 0 {
		t.Fatal("prefetch consumed an incomplete live event")
	}

	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		stream.readLoop(reader)
	}()
	writeDone = make(chan error, 1)
	go func() {
		_, err := server.Write([]byte("}}\n"))
		writeDone <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	live, err := stream.Next(ctx)
	cancel()
	if err != nil {
		t.Fatalf("read live event: %v", err)
	}
	if live.Event != "pane.created" {
		t.Fatalf("live event = %#v, want pane.created", live)
	}
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("write live event: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("live event write did not finish")
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close stream: %v", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	_, err = stream.Next(ctx)
	cancel()
	if err == nil {
		t.Fatal("live event was delivered more than once")
	}
	select {
	case <-loopDone:
	case <-time.After(time.Second):
		t.Fatal("event reader did not stop after close")
	}
}

func TestEventStreamStopsAfterBufferedDecodeError(t *testing.T) {
	source := &countingEventReader{data: []byte(
		`{"event":"pane_closed","data":{"pane_id":"before"}}` + "\n" +
			`{"event":` + "\n",
	)}
	reader := bufio.NewReader(source)
	if _, err := reader.Peek(1); err != nil {
		t.Fatalf("prime reader: %v", err)
	}
	stream := &EventStream{queue: newEventQueue()}
	if stream.startReader(reader) {
		t.Fatal("reader started after buffered decode failure")
	}
	before, err := stream.Next(context.Background())
	if err != nil {
		t.Fatalf("read event before decode failure: %v", err)
	}
	if before.Event != "pane.closed" {
		t.Fatalf("event before decode failure = %#v", before)
	}
	_, err = stream.Next(context.Background())
	if err == nil || !strings.Contains(err.Error(), "decode Herdr event") {
		t.Fatalf("terminal stream error = %v, want decode error", err)
	}
	if source.reads != 1 {
		t.Fatalf("reader reads = %d, want 1 with no read after terminal error", source.reads)
	}
}

func TestEventBootstrapFallsBackFromUnsupportedOptionalSubscription(t *testing.T) {
	releaseLive := make(chan struct{})
	var releaseLiveOnce sync.Once
	releaseLiveNow := func() { releaseLiveOnce.Do(func() { close(releaseLive) }) }
	t.Cleanup(releaseLiveNow)
	firstConn := newControlledEventConn(testJSONBatch(t, map[string]any{
		"id": "",
		"error": map[string]any{
			"code":    "invalid_request",
			"message": "invalid request: unknown variant `workspace.reordered`, expected `workspace.created` or `workspace.moved`",
		},
	}), nil, nil)
	eventConn := newControlledEventConn(
		testJSONBatch(t,
			map[string]any{
				"id":     eventSubscriptionRequestID,
				"result": map[string]any{"type": "subscription_started"},
			},
			map[string]any{
				"event": "pane_closed",
				"data":  map[string]any{"type": "pane_closed", "pane_id": "pane-live"},
			},
		),
		testJSONBatch(t, map[string]any{
			"event": "pane_created",
			"data":  map[string]any{"type": "pane_created", "pane_id": "pane-fallback-live"},
		}),
		releaseLive,
	)
	snapshotConn := newControlledEventConn(testJSONBatch(t, map[string]any{
		"id": "mobile-relay-snapshot",
		"result": map[string]any{
			"type":     "session_snapshot",
			"snapshot": map[string]any{"version": "0.9.0", "protocol": 1},
		},
	}), nil, nil)
	client := newControlledEventClient(firstConn, eventConn, snapshotConn)
	var supported, unsupported int
	client.SetWorkspaceReorderedCapability(
		func() bool { return true },
		func() { supported++ },
		func() { unsupported++ },
	)
	stream, snapshot, buffered, err := client.Bootstrap(context.Background())
	if err != nil {
		t.Fatalf("Bootstrap() error = %v", err)
	}
	if snapshot.Protocol != 1 || len(buffered) != 1 || buffered[0].Event != "pane.closed" {
		t.Fatalf("snapshot=%+v buffered=%+v", snapshot, buffered)
	}
	releaseLiveNow()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	live, err := stream.Next(ctx)
	cancel()
	if err != nil || live.Event != "pane.created" {
		t.Fatalf("fallback live event = %#v, err=%v, want one pane.created", live, err)
	}
	select {
	case <-eventConn.eofRead:
	case <-time.After(time.Second):
		t.Fatal("fallback event reader did not observe EOF")
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	_, err = stream.Next(ctx)
	cancel()
	if err == nil {
		t.Fatal("fallback live event was delivered more than once")
	}
	if supported != 0 || unsupported != 1 {
		t.Fatalf("capability callbacks supported=%d unsupported=%d", supported, unsupported)
	}
	first := controlledSubscriptionTypes(t, firstConn)
	second := controlledSubscriptionTypes(t, eventConn)
	if !containsString(first, "workspace.reordered") {
		t.Fatalf("first subscription = %v", first)
	}
	if containsString(second, "workspace.reordered") {
		t.Fatalf("fallback subscription retained optional event: %v", second)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close fallback stream: %v", err)
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
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

func TestTopologySubscriptionsMatchHerdr075Contract(t *testing.T) {
	want := []string{
		"pane.created",
		"pane.closed",
		"pane.updated",
		"pane.moved",
		"pane.exited",
		"pane.agent_detected",
		"tab.created",
		"tab.closed",
		"tab.renamed",
		"tab.moved",
		"workspace.created",
		"workspace.updated",
		"workspace.metadata_updated",
		"workspace.closed",
		"workspace.renamed",
		"workspace.moved",
		"workspace.focused",
		"worktree.created",
		"worktree.opened",
		"worktree.removed",
	}
	got := topologySubscriptions(false)
	if len(got) != len(want) {
		t.Fatalf("subscription count = %d, want %d", len(got), len(want))
	}
	for index, subscription := range got {
		if subscription["type"] != want[index] {
			t.Fatalf("subscription %d = %q, want %q", index, subscription["type"], want[index])
		}
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

type countingEventReader struct {
	data  []byte
	reads int
}

func (r *countingEventReader) Read(data []byte) (int, error) {
	r.reads++
	if r.reads > 1 {
		return 0, fmt.Errorf("read after terminal event error")
	}
	return copy(data, r.data), nil
}

type controlledEventConn struct {
	mu       sync.Mutex
	initial  []byte
	live     []byte
	release  <-chan struct{}
	closed   chan struct{}
	closeOne sync.Once
	liveRead chan struct{}
	eofRead  chan struct{}
	liveSent bool
	eofSent  bool
	writes   [][]byte
}

func newControlledEventConn(initial, live []byte, release <-chan struct{}) *controlledEventConn {
	return &controlledEventConn{
		initial:  append([]byte(nil), initial...),
		live:     append([]byte(nil), live...),
		release:  release,
		closed:   make(chan struct{}),
		liveRead: make(chan struct{}),
		eofRead:  make(chan struct{}),
	}
}

func (c *controlledEventConn) Read(data []byte) (int, error) {
	c.mu.Lock()
	select {
	case <-c.closed:
		c.mu.Unlock()
		return 0, io.ErrClosedPipe
	default:
	}
	if len(c.initial) > 0 {
		n := copy(data, c.initial)
		c.initial = c.initial[n:]
		c.mu.Unlock()
		return n, nil
	}
	if !c.liveSent && len(c.live) > 0 {
		c.liveSent = true
		live := append([]byte(nil), c.live...)
		release := c.release
		closed := c.closed
		liveRead := c.liveRead
		c.mu.Unlock()
		if release != nil {
			select {
			case <-release:
			case <-closed:
				return 0, io.ErrClosedPipe
			}
		}
		close(liveRead)
		return copy(data, live), nil
	}
	if !c.eofSent {
		c.eofSent = true
		eofRead := c.eofRead
		c.mu.Unlock()
		close(eofRead)
		return 0, io.EOF
	}
	c.mu.Unlock()
	return 0, io.EOF
}

func (c *controlledEventConn) Write(data []byte) (int, error) {
	c.mu.Lock()
	c.writes = append(c.writes, append([]byte(nil), data...))
	c.mu.Unlock()
	return len(data), nil
}

func (c *controlledEventConn) Close() error {
	c.closeOne.Do(func() { close(c.closed) })
	return nil
}

func (c *controlledEventConn) LocalAddr() net.Addr              { return controlledEventAddr("local") }
func (c *controlledEventConn) RemoteAddr() net.Addr             { return controlledEventAddr("remote") }
func (c *controlledEventConn) SetDeadline(time.Time) error      { return nil }
func (c *controlledEventConn) SetReadDeadline(time.Time) error  { return nil }
func (c *controlledEventConn) SetWriteDeadline(time.Time) error { return nil }

type controlledEventAddr string

func (a controlledEventAddr) Network() string { return "controlled" }
func (a controlledEventAddr) String() string  { return string(a) }

func newControlledEventClient(conns ...*controlledEventConn) *EventClient {
	client := NewEventClient("controlled")
	var mu sync.Mutex
	next := 0
	client.dialContext = func(context.Context, string, string) (net.Conn, error) {
		mu.Lock()
		defer mu.Unlock()
		if next >= len(conns) {
			return nil, fmt.Errorf("controlled dialer exhausted")
		}
		conn := conns[next]
		next++
		return conn, nil
	}
	return client
}

func controlledSubscriptionTypes(t *testing.T, conn *controlledEventConn) []string {
	t.Helper()
	conn.mu.Lock()
	if len(conn.writes) != 1 {
		writes := len(conn.writes)
		conn.mu.Unlock()
		t.Fatalf("subscription writes = %d, want 1", writes)
	}
	request := append([]byte(nil), conn.writes[0]...)
	conn.mu.Unlock()
	var decoded struct {
		Params struct {
			Subscriptions []map[string]string `json:"subscriptions"`
		} `json:"params"`
	}
	if err := json.Unmarshal(request, &decoded); err != nil {
		t.Fatalf("decode subscription request: %v", err)
	}
	types := make([]string, 0, len(decoded.Params.Subscriptions))
	for _, subscription := range decoded.Params.Subscriptions {
		types = append(types, subscription["type"])
	}
	return types
}

func testJSONBatch(t *testing.T, values ...any) []byte {
	t.Helper()
	payload, err := marshalTestJSONBatch(values...)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func writeTestJSON(conn net.Conn, value any) error {
	return writeTestJSONBatch(conn, value)
}

func writeTestJSONBatch(conn net.Conn, values ...any) error {
	payload, err := marshalTestJSONBatch(values...)
	if err != nil {
		return err
	}
	_, err = conn.Write(payload)
	return err
}

func marshalTestJSONBatch(values ...any) ([]byte, error) {
	payload := make([]byte, 0)
	for _, value := range values {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		payload = append(payload, encoded...)
		payload = append(payload, '\n')
	}
	return payload, nil
}
