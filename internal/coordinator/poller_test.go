package coordinator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func TestReconcileSynchronouslyCommitsFreshInventory(t *testing.T) {
	root := t.TempDir()
	fakeHerdr := filepath.Join(root, "herdr")
	if err := os.WriteFile(fakeHerdr, []byte(`#!/bin/sh
case "$*" in
  "agent list") printf '%s\n' '{"result":{"agents":[]}}' ;;
  "workspace list") printf '%s\n' '{"result":{"workspaces":[]}}' ;;
  "tab list") printf '%s\n' '{"result":{"tabs":[]}}' ;;
  "pane list") printf '%s\n' '{"result":{"panes":[]}}' ;;
  *) exit 1 ;;
esac
`), 0o700); err != nil {
		t.Fatal(err)
	}
	state := testState()
	poller := NewPoller(herdr.NewClient(fakeHerdr, filepath.Join(root, "herdr.sock")), state, time.Second, testLogger())
	if err := poller.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ready := state.SnapshotAtReadyInventory(); !ready {
		t.Fatal("synchronous reconcile returned before the inventory committed")
	}
}

func TestReconcileReturnsTheInventoryFailure(t *testing.T) {
	root := t.TempDir()
	fakeHerdr := filepath.Join(root, "herdr")
	if err := os.WriteFile(fakeHerdr, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	state := testState()
	poller := NewPoller(herdr.NewClient(fakeHerdr, filepath.Join(root, "herdr.sock")), state, time.Second, testLogger())
	if err := poller.Reconcile(t.Context()); err == nil {
		t.Fatal("synchronous reconcile hid the inventory failure")
	}
	if state.InventoryReady() {
		t.Fatal("failed synchronous reconcile left the inventory ready")
	}
}

func TestReconcileReturnsWorkspaceFailureAndExhaustsTopologyChurn(t *testing.T) {
	t.Run("workspace failure", func(t *testing.T) {
		root := t.TempDir()
		fakeHerdr := writeScript(t, root, "herdr", "#!/bin/sh\ncase \"$1 $2\" in\n  'agent list') printf '%s\\n' '{\"result\":{\"agents\":[]}}' ;;\n  'workspace list') exit 1 ;;\n  *) printf '%s\\n' '{\"result\":{}}' ;;\nesac\n")
		poller := NewPoller(herdr.NewClient(fakeHerdr, filepath.Join(root, "herdr.sock")), testState(), time.Second, testLogger())
		if err := poller.Reconcile(t.Context()); err == nil {
			t.Fatal("workspace inventory failure was hidden")
		}
	})

	t.Run("topology churn", func(t *testing.T) {
		root := t.TempDir()
		fakeHerdr := writeScript(t, root, "herdr", "#!/bin/sh\ncase \"$1 $2\" in\n  'agent list') printf '%s\\n' '{\"result\":{\"agents\":[]}}' ;;\n  'workspace list') printf '%s\\n' '{\"result\":{\"workspaces\":[]}}' ;;\n  'tab list') printf '%s\\n' '{\"result\":{\"tabs\":[]}}' ;;\n  'pane list') printf '%s\\n' '{\"result\":{\"panes\":[]}}' ;;\n  *) exit 1 ;;\nesac\n")
		state := testState()
		poller := NewPoller(herdr.NewClient(fakeHerdr, filepath.Join(root, "herdr.sock")), state, time.Second, testLogger())
		poller.SetEnrich(func(context.Context, []*AgentState) { state.MarkTopologyChanged() })
		if err := poller.Reconcile(t.Context()); !errors.Is(err, errTopologyStale) {
			t.Fatalf("topology churn = %v, want %v", err, errTopologyStale)
		}
	})
}

func TestPollerRunCoversWakeTimerAndCancellation(t *testing.T) {
	root := t.TempDir()
	fakeHerdr := writeScript(t, root, "herdr", "#!/bin/sh\ncase \"$1 $2\" in\n  'agent list') printf '%s\\n' '{\"result\":{\"agents\":[]}}' ;;\n  'workspace list') printf '%s\\n' '{\"result\":{\"workspaces\":[]}}' ;;\n  'tab list') printf '%s\\n' '{\"result\":{\"tabs\":[]}}' ;;\n  'pane list') printf '%s\\n' '{\"result\":{\"panes\":[]}}' ;;\n  *) exit 1 ;;\nesac\n")
	poller := NewPoller(herdr.NewClient(fakeHerdr, filepath.Join(root, "herdr.sock")), testState(), 100*time.Millisecond, testLogger())
	commits := make(chan struct{}, 4)
	poller.SetAfterCommit(func(context.Context, []*AgentState) { commits <- struct{}{} })
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		poller.Run(ctx)
		close(done)
	}()
	waitForPollerCommit(t, commits)
	poller.Wake()
	waitForPollerCommit(t, commits)
	waitForPollerCommit(t, commits)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("poller did not stop after cancellation")
	}
}

func TestForegroundReconcileSerializesWithBackgroundPoll(t *testing.T) {
	root := t.TempDir()
	fakeHerdr := writeScript(t, root, "herdr", "#!/bin/sh\ncase \"$1 $2\" in\n  'agent list') printf '%s\\n' '{\"result\":{\"agents\":[]}}' ;;\n  'workspace list') printf '%s\\n' '{\"result\":{\"workspaces\":[]}}' ;;\n  'tab list') printf '%s\\n' '{\"result\":{\"tabs\":[]}}' ;;\n  'pane list') printf '%s\\n' '{\"result\":{\"panes\":[]}}' ;;\n  *) exit 1 ;;\nesac\n")
	poller := NewPoller(herdr.NewClient(fakeHerdr, filepath.Join(root, "herdr.sock")), testState(), time.Second, testLogger())
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	var enrichments atomic.Int32
	poller.SetEnrich(func(context.Context, []*AgentState) {
		if enrichments.Add(1) == 1 {
			close(firstEntered)
			<-releaseFirst
		}
	})
	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)
	go func() { firstDone <- poller.poll(t.Context()) }()
	<-firstEntered
	go func() { secondDone <- poller.Reconcile(t.Context()) }()
	time.Sleep(50 * time.Millisecond)
	if got := enrichments.Load(); got != 1 {
		t.Fatalf("concurrent poll entered enrichment %d times before the first completed", got)
	}
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	if got := enrichments.Load(); got != 2 {
		t.Fatalf("serialized poll enrichments = %d, want 2", got)
	}
}

func waitForPollerCommit(t *testing.T, commits <-chan struct{}) {
	t.Helper()
	select {
	case <-commits:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for poll commit")
	}
}

func TestRejectedPollCannotPublishPostCommitEnrichment(t *testing.T) {
	state := testState()
	state.CommitInventory([]*AgentState{{PaneID: "pane-current", SessionID: "session-current", Status: "idle"}}, 0)
	poller := NewPoller(nil, state, time.Second, testLogger())
	published := 0
	poller.SetAfterCommit(func(context.Context, []*AgentState) { published++ })
	stale := state.BeginPoll()
	state.CommitTopology(
		[]*AgentState{{PaneID: "pane-current", SessionID: "session-current", Status: "idle"}},
		[]herdr.Workspace{{ID: "workspace-new", Label: "New"}},
		state.RevisionCounter(),
	)
	if _, committed := poller.commitPoll(context.Background(), []*AgentState{{PaneID: "pane-stale", SessionID: "session-stale"}}, nil, stale); committed {
		t.Fatal("topology-stale poll committed")
	}
	if published != 0 {
		t.Fatalf("stale enrichment publications = %d, want 0", published)
	}

	fresh := state.BeginPoll()
	if _, committed := poller.commitPoll(context.Background(), []*AgentState{{PaneID: "pane-current", SessionID: "session-current"}}, nil, fresh); !committed {
		t.Fatal("fresh poll was rejected")
	}
	if published != 1 {
		t.Fatalf("fresh enrichment publications = %d, want 1", published)
	}
}

func TestPollAndEventSerializeCommitWhileOnlyPollPublishesAuthoritativePostCommit(t *testing.T) {
	state := testState()
	poller := NewPoller(nil, state, time.Second, testLogger())
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	var orderMu sync.Mutex
	var order []string
	poller.SetAfterCommit(func(_ context.Context, agents []*AgentState) {
		paneID := agents[0].PaneID
		orderMu.Lock()
		order = append(order, paneID)
		orderMu.Unlock()
		if paneID == "pane-old" {
			close(firstEntered)
			<-releaseFirst
			return
		}
	})

	token := state.BeginPoll()
	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		poller.commitPoll(context.Background(), []*AgentState{{PaneID: "pane-old", SessionID: "session-old"}}, nil, token)
	}()
	<-firstEntered
	eventDone := make(chan struct{})
	go func() {
		defer close(eventDone)
		poller.commitEventTopology(context.Background(), herdr.TopologySnapshot{Panes: []herdr.Pane{{ID: "pane-new", Session: "session-new", Agent: "codex"}}}, state.RevisionCounter())
	}()
	time.Sleep(50 * time.Millisecond)
	close(releaseFirst)
	<-pollDone
	<-eventDone
	orderMu.Lock()
	defer orderMu.Unlock()
	if !reflect.DeepEqual(order, []string{"pane-old"}) {
		t.Fatalf("publication order = %v", order)
	}
}

func TestEventCommitCannotDriveAuthoritativeOwnershipReconciliation(t *testing.T) {
	state := testState()
	poller := NewPoller(nil, state, time.Second, testLogger())
	calls := 0
	poller.SetAfterCommit(func(context.Context, []*AgentState) { calls++ })
	poller.commitEventTopology(context.Background(), herdr.TopologySnapshot{
		Panes: []herdr.Pane{{ID: "pane-event", Session: "session-event", Agent: "codex"}},
	}, state.RevisionCounter())
	if calls != 0 {
		t.Fatalf("event-driven authoritative post-commit calls = %d, want 0", calls)
	}
}

func TestEventCommitAllowsNoPostCommitHook(t *testing.T) {
	state := testState()
	poller := NewPoller(nil, state, time.Second, testLogger())
	poller.commitEventTopology(context.Background(), herdr.TopologySnapshot{}, state.RevisionCounter())
}

func TestTopologyStaleRepollsAreBounded(t *testing.T) {
	state := testState()
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Status: "working"}}, 0)
	poller := NewPoller(nil, state, time.Second, testLogger())

	for retry := 0; retry < maxImmediateTopologyPolls; retry++ {
		poller.handleTopologyStale(state.InventoryStatus())
		select {
		case <-poller.wakeup:
		default:
			t.Fatalf("retry %d did not request an immediate repoll", retry+1)
		}
	}
	poller.handleTopologyStale(state.InventoryStatus())
	select {
	case <-poller.wakeup:
		t.Fatal("topology churn requested an unbounded immediate repoll")
	default:
	}
	status := state.InventoryStatus()
	if status["state"] != "error" || status["error_code"] != "topology_churn" {
		t.Fatalf("inventory status = %+v, want topology degradation", status)
	}
}

// While the event stream is healthy the poll is only a reconcile backstop, but
// when events are unavailable it is the sole freshness source and must honour
// the operator-configured interval again.
func TestPollerIntervalTracksEventStreamHealth(t *testing.T) {
	state := testState()
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Status: "working"}}, 0)
	poller := NewPoller(nil, state, time.Second, testLogger())

	if got := poller.currentInterval(); got != time.Second {
		t.Fatalf("interval with events down = %v, want the configured 1s", got)
	}

	poller.eventsActive.Store(true)
	if got := poller.currentInterval(); got != idlePollInterval {
		t.Fatalf("interval with events up = %v, want %v", got, idlePollInterval)
	}

	poller.eventsActive.Store(false)
	if got := poller.currentInterval(); got != time.Second {
		t.Fatalf("interval after events dropped = %v, want the configured 1s", got)
	}
}

func TestPollerIntervalClampsToReconcileCeiling(t *testing.T) {
	poller := NewPoller(nil, testState(), time.Hour, testLogger())
	if got := poller.currentInterval(); got != idlePollInterval {
		t.Fatalf("interval = %v, want it clamped to %v", got, idlePollInterval)
	}
}

// An idle machine commits an identical inventory every reconcile interval;
// re-broadcasting it hands every phone a fresh full snapshot to re-render for
// no reason. Only a snapshot that differs from the last broadcast one may go
// out; an explicit refresh_agents request is answered separately.
func TestPollerSkipsUnchangedAgentBroadcasts(t *testing.T) {
	state := testState()
	poller := NewPoller(nil, state, time.Second, testLogger())
	broadcasts := 0
	poller.SetOnChange(func([]*AgentState) { broadcasts++ })

	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Status: "idle"}}, 0)
	poller.notifyAgentsChanged()
	poller.notifyAgentsChanged()
	poller.notifyAgentsChanged()
	if broadcasts != 1 {
		t.Fatalf("broadcasts after identical snapshots = %d, want 1", broadcasts)
	}

	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Status: "working"}}, state.RevisionCounter())
	poller.notifyAgentsChanged()
	if broadcasts != 2 {
		t.Fatalf("broadcasts after a real change = %d, want 2", broadcasts)
	}
}

// Workspace broadcasts read the snapshot under the ordering lock and skip a
// byte-identical repeat, so the reconcile poll and the event stream cannot
// publish a stale topology over a newer one or re-push what clients already
// display.
func TestNotifyWorkspacesChangedSkipsIdenticalTopology(t *testing.T) {
	state := testState()
	poller := NewPoller(nil, state, time.Second, testLogger())
	broadcasts := 0
	poller.SetOnWorkspaceChange(func(workspaces []herdr.Workspace) { broadcasts++ })

	state.CommitWorkspaces([]herdr.Workspace{{ID: "w1", Label: "One"}})
	poller.notifyWorkspacesChanged()
	poller.notifyWorkspacesChanged()
	if broadcasts != 1 {
		t.Fatalf("broadcasts after identical topologies = %d, want 1", broadcasts)
	}

	state.CommitWorkspaces([]herdr.Workspace{{ID: "w1", Label: "Renamed"}})
	poller.notifyWorkspacesChanged()
	if broadcasts != 2 {
		t.Fatalf("broadcasts after a real change = %d, want 2", broadcasts)
	}
}

func TestHydrateWorkspaceCwdsKeepsShellOnlyWorkspaceLaunchable(t *testing.T) {
	workspaces := []herdr.Workspace{{ID: "w1", Label: "Shell only"}}
	hydrateWorkspaceCwds(workspaces, nil, []herdr.Pane{{
		ID: "p1", WorkspaceID: "w1", Cwd: "/home/user/project",
	}})
	if workspaces[0].Cwd != "/home/user/project" {
		t.Fatalf("workspace cwd = %q", workspaces[0].Cwd)
	}
}
