package app

import (
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"testing"
)

func TestRemoteRequestSafety(t *testing.T) {
	for _, action := range []string{"send_prompt", "send_input", "respond", "agent_stop", "agent_start", "get_conversation_history", "workspace_file", "list_slash_commands", "copy_agent_response", "upload_begin", "lease_pane_size", "unknown_future_action"} {
		for _, request := range []protocol.Inbound{
			{Type: action, PaneID: "remote/m/w1:p1"},
			{Type: action, Target: &protocol.TargetRef{PaneID: "remote/m/w1:p1"}},
			{Type: action, Target: &protocol.TargetRef{TerminalID: "remote/m/term1"}},
		} {
			if !remoteRequestDenied(request) {
				t.Fatalf("accepted remote request %#v", request)
			}
		}
		if remoteRequestDenied(protocol.Inbound{Type: action, PaneID: "w1:p1"}) {
			t.Fatalf("local %s denied", action)
		}
	}
	for _, action := range []string{"read_pane", "watch_pane", "unwatch_pane", "pane_applied"} {
		if remoteRequestDenied(protocol.Inbound{Type: action, PaneID: "remote/m/w1:p1"}) {
			t.Fatalf("read-only action %s denied", action)
		}
	}
	for _, request := range []protocol.Inbound{
		{WorkspaceID: "remote/m/w1"}, {BeforeWorkspaceID: "remote/m/w1"},
		{WorkspaceIDs: []string{"w1", "remote/m/w1"}}, {ExpectedWorkspaceIDs: []string{"remote/m/w1"}},
	} {
		if !remoteRequestDenied(request) {
			t.Fatalf("accepted remote workspace operation %#v", request)
		}
	}
}

func TestRemoteSessionsNeverResolveLocally(t *testing.T) {
	// No local session resolver: a call into local transcript discovery would panic.
	server := &Server{}
	agent := &coordinator.AgentState{PaneID: "remote/m/w1:p1", Agent: "claude", Session: "remote-session", Cwd: "/local/collision"}
	server.resolveAgentSessionName(agent)
	if agent.ConversationHistoryAvailable || agent.SessionID != "" || agent.SessionName != "" {
		t.Fatal("remote transcript was advertised as local")
	}
	server.handleTransition(t.Context(), agent.PaneID, "claude", "project", "idle", 1)
}
