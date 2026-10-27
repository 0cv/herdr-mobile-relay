package coordinator

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func TestTopologyCacheCannotRevertPolledNativeSession(t *testing.T) {
	for _, cachedSession := range []string{"", "previous-session"} {
		t.Run(cachedSession, func(t *testing.T) {
			pane := herdr.Pane{ID: "pane", TerminalID: "terminal", WorkspaceID: "workspace", TabID: "tab", Agent: "pi", Status: "idle"}
			var session atomic.Value
			session.Store("current-session")
			var active atomic.Bool
			active.Store(true)
			socket := startPollerTestSocket(t, func(method string) (any, string) {
				if method == "agent.list" {
					if !active.Load() {
						return map[string]any{"type": "agent_list", "agents": []herdr.Pane{}}, ""
					}
					current := pane
					current.SessionRaw.Value = session.Load().(string)
					return map[string]any{"type": "agent_list", "agents": []herdr.Pane{current}}, ""
				}
				return pollerSuccessResult(method), ""
			})
			client := herdr.NewClient("unused", socket)
			t.Cleanup(func() { _ = client.Close() })
			state := testState()
			state.CommitInventory([]*AgentState{{PaneID: pane.ID, TerminalID: pane.TerminalID, Agent: pane.Agent, Status: "idle"}}, 0)
			poller := NewPoller(client, state, time.Second, testLogger())
			poller.SetEnrich(func(_ context.Context, agents []*AgentState) {
				for _, agent := range agents {
					agent.SessionID = agent.Session
					agent.AgentSessionID = agent.Session
				}
			})
			poller.poll(context.Background())
			before, _ := state.Agent("pane")
			cached := pane
			cached.Session = cachedSession
			cached.SessionRaw.Value = cachedSession
			for range 3 {
				poller.commitEventTopology(context.Background(), herdr.TopologySnapshot{Panes: []herdr.Pane{cached}}, state.RevisionCounter())
				after, ok := state.Agent("pane")
				if !ok || after.SessionID != before.SessionID || after.Generation != before.Generation {
					t.Fatalf("topology event changed the live identity: before=%+v after=%+v", before, after)
				}
				poller.poll(context.Background())
			}
			for _, next := range []string{"next-session", ""} {
				before, _ = state.Agent("pane")
				session.Store(next)
				poller.commitEventTopology(context.Background(), herdr.TopologySnapshot{Panes: []herdr.Pane{cached}}, state.RevisionCounter())
				after, ok := state.Agent("pane")
				if !ok || after.SessionID != next || after.Generation != before.Generation+1 {
					t.Fatalf("live session change was not fenced: before=%+v after=%+v", before, after)
				}
			}
			active.Store(false)
			poller.commitEventTopology(context.Background(), herdr.TopologySnapshot{Panes: []herdr.Pane{cached}}, state.RevisionCounter())
			if agent, ok := state.Agent("pane"); ok {
				t.Fatalf("cached topology resurrected a departed agent: %+v", agent)
			}
		})
	}
}

func TestEventInventoryFailureDoesNotPublishCachedSession(t *testing.T) {
	socket := startPollerTestSocket(t, func(string) (any, string) {
		return nil, "internal_error"
	})
	client := herdr.NewClient("unused", socket)
	t.Cleanup(func() { _ = client.Close() })
	state := testState()
	state.CommitInventory([]*AgentState{{PaneID: "pane", TerminalID: "terminal", SessionID: "current-session", Agent: "pi", Status: "idle"}}, 0)
	before, _ := state.Agent("pane")
	poller := NewPoller(client, state, time.Second, testLogger())
	poller.commitEventTopology(context.Background(), herdr.TopologySnapshot{Panes: []herdr.Pane{{ID: "pane", TerminalID: "terminal", Agent: "pi"}}}, state.RevisionCounter())
	after, ok := state.Agent("pane")
	if !ok || after.SessionID != before.SessionID || after.Generation != before.Generation {
		t.Fatalf("failed refresh published stale identity: before=%+v after=%+v", before, after)
	}
	if state.InventoryStatus()["state"] != "error" {
		t.Fatal("failed refresh reported ready inventory")
	}
	select {
	case <-poller.wakeup:
	default:
		t.Fatal("failed refresh did not schedule reconciliation")
	}
}
