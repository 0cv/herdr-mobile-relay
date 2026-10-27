package coordinator

import (
	"context"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func TestPiPromptAdmissionAndNoTerminalFallback(t *testing.T) {
	for _, scenario := range []string{"valid", "secret", "read-failed", "session-replaced", "working", "unblocked", "approval", "other-provider"} {
		t.Run(scenario, func(t *testing.T) {
			state := NewState(testLogger())
			agent := &AgentState{PaneID: "pane-1", Agent: "pi", Status: "blocked", SessionID: "session", AttentionKind: "unknown"}
			if scenario == "working" || scenario == "unblocked" {
				agent.Status = scenario
			}
			if scenario == "approval" {
				agent.AttentionKind = "approval"
			}
			if scenario == "other-provider" {
				agent.Agent = "codex"
			}
			state.CommitInventory([]*AgentState{agent}, state.RevisionCounter())
			content := "Unrecognized dialog"
			if scenario == "secret" {
				content = "Password:"
			}
			var changed func()
			if scenario == "session-replaced" {
				changed = func() { state.BumpGeneration("pane-1") }
			}
			client := herdr.NewClient("/nonexistent-private-fixture-command", secretReadSocket(t, content, scenario == "read-failed", changed))
			defer client.Close()
			dispatcher := NewDispatcher(client, state, nil, testLogger())
			defer dispatcher.Close(context.Background())
			calls, admissions := 0, 0
			result := dispatcher.HandlePiPrompt(context.Background(), "req-1", "pane-1", func() { admissions++ }, func(ctx context.Context) *CommandResult {
				calls++
				return &CommandResult{Action: "send_pi_prompt_unconfirmed", OK: true, Phase: "unconfirmed"}
			})
			if (calls == 1) != (scenario == "valid") || (result.OK != (scenario == "valid")) {
				t.Fatalf("%s: calls=%d result=%+v", scenario, calls, result)
			}
			if admissions != 1 {
				t.Fatalf("admissions=%d", admissions)
			}
		})
	}
}
