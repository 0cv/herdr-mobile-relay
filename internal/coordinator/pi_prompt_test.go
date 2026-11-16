package coordinator

import (
	"context"
	"errors"
	"testing"
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
			client := newFakeBackend()
			client.contents["pane-1"] = content
			if scenario == "read-failed" {
				client.readErr = errors.New("controlled read failure")
			}
			if scenario == "session-replaced" {
				client.setHook("ReadPaneVisible", func(context.Context, fakeCall) error {
					state.BumpGeneration("pane-1")
					return nil
				})
			}
			dispatcher := newTestDispatcher(t, client, state)
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
