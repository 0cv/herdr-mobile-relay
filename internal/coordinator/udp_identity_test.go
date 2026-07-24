package coordinator

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"
)

func TestUDPListenerRejectsEventFromSameIDPreviousPaneSession(t *testing.T) {
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{
		PaneID: "pane-1", RawPaneID: "pane-1", TerminalID: "terminal-old",
		TabID: "tab-old", WorkspaceID: "workspace-old", Status: "working",
	}}, state.RevisionCounter())
	state.CommitInventory([]*AgentState{{
		PaneID: "pane-1", RawPaneID: "pane-1", TerminalID: "terminal-new",
		TabID: "tab-new", WorkspaceID: "workspace-new", Status: "working",
	}}, state.RevisionCounter())

	listener, err := NewUDPListener("127.0.0.1:0", state, "/expected/herdr.sock", testLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		listener.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
		<-done
	})

	payload, err := json.Marshal(map[string]any{
		"type":         "agent_event",
		"socket_path":  "/expected/herdr.sock",
		"pane_id":      "pane-1",
		"tab_id":       "tab-old",
		"workspace_id": "workspace-old",
		"status":       "blocked",
	})
	if err != nil {
		t.Fatal(err)
	}
	connection, err := net.DialUDP("udp", nil, listener.conn.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Write(payload); err != nil {
		connection.Close()
		t.Fatal(err)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(time.Second)
	for listener.Metrics().Received == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if listener.Metrics().Received == 0 {
		t.Fatal("UDP event was not received")
	}
	agent, ok := state.Agent("pane-1")
	if !ok {
		t.Fatal("replacement pane disappeared")
	}
	if agent.Status != "working" {
		t.Fatalf("stale old-session event changed replacement status to %q", agent.Status)
	}
}

func TestPendingUDPEventIsScopedToPaneSession(t *testing.T) {
	state := NewState(testLogger())
	if state.CommitEventForSession(
		"pane-1", "tab-old", "workspace-old", "blocked", time.Now().UnixMilli(),
	) {
		t.Fatal("event for absent pane unexpectedly committed")
	}
	state.CommitInventory([]*AgentState{{
		PaneID: "pane-1", RawPaneID: "pane-1", TerminalID: "terminal-new",
		TabID: "tab-new", WorkspaceID: "workspace-new", Status: "working",
	}}, state.RevisionCounter())

	agent, ok := state.Agent("pane-1")
	if !ok {
		t.Fatal("replacement pane missing")
	}
	if agent.Status != "working" {
		t.Fatalf("pending old-session event changed replacement status to %q", agent.Status)
	}
}

func TestIdentitylessUDPEventRemainsCompatible(t *testing.T) {
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{
		PaneID: "pane-1", TabID: "tab-1", WorkspaceID: "workspace-1", Status: "working",
	}}, state.RevisionCounter())

	if !state.CommitEvent("pane-1", "blocked", time.Now().UnixMilli()) {
		t.Fatal("identity-less event was not committed")
	}
	agent, _ := state.Agent("pane-1")
	if agent.Status != "blocked" {
		t.Fatalf("identity-less event status = %q, want blocked", agent.Status)
	}
}
