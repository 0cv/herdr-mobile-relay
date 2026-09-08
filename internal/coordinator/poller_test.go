package coordinator

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
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
	updates := make(chan []herdr.Workspace, 8)
	poller.SetOnWorkspaceChange(func(workspaces []herdr.Workspace) {
		updates <- append([]herdr.Workspace(nil), workspaces...)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan struct{})
	go func() {
		poller.RunEvents(ctx, herdr.NewEventClient(socketPath))
		close(runDone)
	}()

	var final []herdr.Workspace
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for final == nil {
		select {
		case workspaces := <-updates:
			if len(workspaces) == 2 && workspaces[1].ID == "w3" {
				final = workspaces
			}
		case <-deadline.C:
			t.Fatal("event reconnect did not converge on the current snapshot")
		}
	}
	<-runDone
	if len(final) != 2 || final[0].ID != "w1" || final[1].ID != "w3" {
		t.Fatalf("final workspaces = %+v, want w1 and w3", final)
	}
	if reconnects != 2 {
		t.Fatalf("reconnect waits = %d, want 2", reconnects)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}
