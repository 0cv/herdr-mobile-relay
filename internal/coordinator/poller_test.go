package coordinator

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

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

func TestPollerInventoryChangePublishesCurrentState(t *testing.T) {
	state := testState()
	poller := NewPoller(nil, state, time.Second, testLogger())
	var statuses []string
	poller.SetOnInventoryChange(func() error {
		statuses = append(statuses, state.InventoryStatus()["state"].(string))
		return nil
	})
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Status: "idle"}}, 0)
	poller.notifyInventoryChange()
	state.MarkInventoryFailure(fmt.Errorf("temporary failure"))
	poller.notifyInventoryChange()
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Status: "idle"}}, state.RevisionCounter())
	poller.notifyInventoryChange()
	poller.notifyInventoryChange()
	if got, want := fmt.Sprint(statuses), "[ready error ready ready]"; got != want {
		t.Fatalf("publication states = %s, want %s", got, want)
	}
}

func TestPollerInventoryPublicationSerializesOvertakingChanges(t *testing.T) {
	state := testState()
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Status: "idle"}}, 0)
	poller := NewPoller(nil, state, time.Second, testLogger())
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFirst := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseFirst)
	secondDone := make(chan struct{})
	var mu sync.Mutex
	var statuses []string
	calls := 0
	poller.SetOnInventoryChange(func() error {
		status, _ := state.InventoryStatus()["state"].(string)
		mu.Lock()
		statuses = append(statuses, status)
		calls++
		call := calls
		mu.Unlock()
		if call == 1 {
			close(entered)
			<-release
		} else if call == 2 {
			close(secondDone)
		}
		return nil
	})
	go poller.notifyInventoryChange()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first inventory publication did not enter")
	}
	state.MarkInventoryFailure(fmt.Errorf("overtaking failure"))
	go poller.notifyInventoryChange()
	select {
	case <-secondDone:
		t.Fatal("inventory publication overtook the first callback")
	case <-time.After(20 * time.Millisecond):
	}
	releaseFirst()
	select {
	case <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("second inventory publication did not complete")
	}
	mu.Lock()
	got := append([]string(nil), statuses...)
	mu.Unlock()
	if fmt.Sprint(got) != "[ready error]" {
		t.Fatalf("serialized publication states = %v, want [ready error]", got)
	}
}

func TestInventorySnapshotIsCoherentAndOwned(t *testing.T) {
	state := testState()
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Status: "blocked", Options: []string{"one"}}}, 0)
	state.CommitWorkspaces([]herdr.Workspace{{ID: "w1", Label: "One", Worktree: &herdr.WorkspaceWorktree{CheckoutPath: "/work"}}})
	snapshot := state.InventorySnapshot()
	if len(snapshot.Agents) != 1 || len(snapshot.Agents[0].Options) != 1 {
		t.Fatalf("snapshot agents = %#v", snapshot.Agents)
	}
	snapshot.Status["state"] = "error"
	snapshot.Agents[0].Options[0] = "mutated"
	snapshot.Workspaces[0].Worktree.CheckoutPath = "/mutated"
	fresh := state.InventorySnapshot()
	if fresh.Status["state"] != "ready" || fresh.Agents[0].Options[0] != "one" || fresh.Workspaces[0].Worktree.CheckoutPath != "/work" {
		t.Fatalf("state snapshot was not independently owned: %#v", fresh)
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

func TestRunEventsRefreshesSnapshotAfterDroppedStream(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "herdr.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverDone := make(chan error, 1)
	go func() {
		defer listener.Close()
		subscriptions := 0
		snapshots := 0
		for range 4 {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				serverDone <- acceptErr
				return
			}
			func() {
				defer conn.Close()
				var request struct {
					ID     string `json:"id"`
					Method string `json:"method"`
				}
				if decodeErr := json.NewDecoder(bufio.NewReader(conn)).Decode(&request); decodeErr != nil {
					serverDone <- decodeErr
					return
				}
				switch request.Method {
				case "events.subscribe":
					subscriptions++
					_ = json.NewEncoder(conn).Encode(map[string]any{
						"id": request.ID, "result": map[string]any{"type": "subscription_started"},
					})
					if subscriptions == 1 {
						_ = json.NewEncoder(conn).Encode(map[string]any{
							"event": "workspace.created",
							"data": map[string]any{
								"workspace": map[string]any{"workspace_id": "w2", "label": "Buffered"},
							},
						})
					}
				case "session.snapshot":
					snapshots++
					workspaces := []any{
						map[string]any{"workspace_id": "w1", "label": "Project"},
					}
					if snapshots == 2 {
						workspaces = append(workspaces,
							map[string]any{"workspace_id": "w3", "label": "Created Offline"},
						)
					}
					_ = json.NewEncoder(conn).Encode(map[string]any{
						"id": request.ID,
						"result": map[string]any{
							"type":     "session_snapshot",
							"snapshot": map[string]any{"workspaces": workspaces},
						},
					})
				default:
					serverDone <- fmt.Errorf("unexpected event method %q", request.Method)
				}
			}()
		}
		serverDone <- nil
	}()

	state := testState()
	poller := NewPoller(herdr.NewClient("missing-herdr", socketPath), state, time.Second, testLogger())
	reconnects := 0
	poller.eventReconnectWait = func(context.Context) bool {
		reconnects++
		return reconnects == 1
	}
	updates := make(chan InventorySnapshot, 8)
	poller.SetOnInventoryChange(func() error {
		updates <- state.InventorySnapshot()
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan struct{})
	go func() {
		poller.RunEvents(ctx, herdr.NewEventClient(socketPath))
		close(runDone)
	}()

	var final InventorySnapshot
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for final.Workspaces == nil {
		select {
		case snapshot := <-updates:
			if len(snapshot.Workspaces) == 2 && snapshot.Workspaces[1].ID == "w3" {
				final = snapshot
			}
		case <-deadline.C:
			t.Fatal("event reconnect did not converge on the current snapshot")
		}
	}
	<-runDone
	if len(final.Workspaces) != 2 || final.Workspaces[0].ID != "w1" || final.Workspaces[1].ID != "w3" {
		t.Fatalf("final workspaces = %+v, want w1 and w3", final.Workspaces)
	}
	if reconnects != 2 {
		t.Fatalf("reconnect waits = %d, want 2", reconnects)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}
