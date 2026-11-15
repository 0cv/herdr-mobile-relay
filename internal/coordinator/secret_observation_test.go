package coordinator

import (
	"context"
	"errors"
	"testing"
)

func TestSecretRechecksPromptBeforeAnyWrite(t *testing.T) {
	for _, scenario := range []string{"echo-on", "unknown", "read failure", "replacement", "valid"} {
		t.Run(scenario, func(t *testing.T) {
			client := newFakeBackend()
			state := NewState(testLogger())
			state.CommitInventory([]*AgentState{{PaneID: "pane-1", Agent: "codex", Status: "working"}}, state.RevisionCounter())
			client.contents["pane-1"] = "Password:"
			if scenario == "echo-on" {
				client.contents["pane-1"] = "Password accepted\n$ "
			}
			if scenario == "unknown" {
				client.contents["pane-1"] = ""
			}
			if scenario == "read failure" {
				client.readErr = errors.New("controlled read failure")
			}
			if scenario == "replacement" {
				client.setHook("ReadPaneVisible", func(context.Context, fakeCall) error {
					state.BumpGeneration("pane-1")
					return nil
				})
			}
			d := newTestDispatcher(t, client, state)
			result := d.Handle(context.Background(), map[string]any{"type": "send_secret", "pane_id": "pane-1", "request_id": "observed-secret", "text": "synthetic-value"})
			if scenario == "valid" {
				if !result.OK || client.count("SendSecret") != 1 {
					t.Fatalf("valid dispatch: %+v", result)
				}
				return
			}
			if result.OK {
				t.Fatalf("unverified secret dispatched: %+v", result)
			}
			for _, call := range client.recorded() {
				if call.Method != "ReadPaneVisible" {
					t.Fatalf("unverified secret made a write call: %+v", call)
				}
			}
		})
	}
}
