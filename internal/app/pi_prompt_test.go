package app

import (
	"io"
	"log/slog"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

func TestPiPromptExcludesNonPromptFieldsAndAuditHashes(t *testing.T) {
	message := map[string]any{"type": "send_pi_prompt_unconfirmed", "text": "synthetic-hidden-marker"}
	if !validPiPromptRequest(message) {
		t.Fatal("normal prompt fields refused")
	}
	for _, field := range []string{"no_echo", "secret", "attachments", "images", "keys", "prompt"} {
		message[field] = true
		if validPiPromptRequest(message) {
			t.Fatal("non-prompt field admitted")
		}
		delete(message, field)
	}
	details := auditWriteDetails(message)
	if len(details) != 1 || details["text_bytes"] == nil {
		t.Fatalf("unsafe audit details: %#v", details)
	}
}

func TestPiPromptControllerOnlyAndExactTarget(t *testing.T) {
	for _, operation := range []string{"get_pi_prompt_capability", "send_pi_prompt_unconfirmed"} {
		metadata, ok := protocol.ClassifyAction(operation)
		if !ok {
			t.Fatal("unknown operation")
		}
		for _, role := range []string{"reader", "bootstrap", "controller", ""} {
			for _, authenticated := range []bool{false, true} {
				denied := authorizeAuthenticatedIdentity(transport.AuthenticatedIdentity{Role: role}, authenticated, metadata, "") != nil
				if denied == (authenticated && role == "controller") {
					t.Fatalf("incorrect admission: %s %s %v", operation, role, authenticated)
				}
			}
		}
	}
	state := coordinator.NewState(slog.New(slog.NewTextHandler(io.Discard, nil)))
	state.CommitInventory([]*coordinator.AgentState{{PaneID: "pane", ServerSessionID: "primary", TerminalID: "terminal", SessionID: "session", Agent: "pi", Status: "blocked", AttentionKind: "unknown"}}, state.RevisionCounter())
	agent, _ := state.Agent("pane")
	target := protocol.TargetRef{PaneID: "pane", ServerSessionID: "primary", TerminalID: "terminal", Generation: agent.Generation, AgentSessionID: "session"}
	if validateExactPaneTarget(state, protocol.Inbound{PaneID: "pane", Target: &target}, true) != nil {
		t.Fatal("exact target refused")
	}
	for _, change := range []func(*protocol.TargetRef){
		func(t *protocol.TargetRef) { t.PaneID = "wrong" }, func(t *protocol.TargetRef) { t.ServerSessionID = "wrong" }, func(t *protocol.TargetRef) { t.TerminalID = "wrong" }, func(t *protocol.TargetRef) { t.Generation++ }, func(t *protocol.TargetRef) { t.AgentSessionID = "wrong" },
	} {
		stale := target
		change(&stale)
		if validateExactPaneTarget(state, protocol.Inbound{PaneID: "pane", Target: &stale}, true) == nil {
			t.Fatal("stale target admitted")
		}
	}
}
