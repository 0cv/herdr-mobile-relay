package coordinator

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func TestAgentNotReadyOutcomeDependsOnOperation(t *testing.T) {
	for _, action := range []string{"submit_prompt", "agent_start"} {
		t.Run(action, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			var err error
			wantPhase := "not_started"
			wantMessage := "Agent is not ready to receive prompts; review the pane before retrying"
			var fake *fakeBackend
			if action == "agent_start" {
				fake = newFakeBackend()
				fake.setHook("StartAgent", func(context.Context, fakeCall) error {
					return &herdr.OutcomeError{Started: true, Err: &herdr.CLIError{
						Code:    "agent_not_ready",
						Message: "agent reviewer is blocked during startup and is not ready for prompts",
					}}
				})
				lifecycle := &Lifecycle{herdr: fake}
				err = lifecycle.startKindAgent(ctx, "claude", "reviewer", "pane-1", nil)
				wantPhase = "dispatched_unknown"
				wantMessage = "Command may have executed; review the agent before retrying"
			} else {
				// The client marks a prompt refusal through an unexported field, so only a real client can produce it.
				dir := t.TempDir()
				bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
					"printf '%s' '{\"error\":{\"code\":\"agent_not_ready\",\"message\":\"agent pane-1 is not an active named agent\"}}' >&2\n"+
					"exit 1\n")
				client := herdr.NewClient(bin, filepath.Join(dir, "herdr.sock"))
				t.Cleanup(func() { _ = client.Close() })
				err = client.Prompt(ctx, "pane-1", "hello")
			}
			if err == nil {
				t.Fatal("expected agent_not_ready error")
			}
			if !errors.Is(err, herdr.ErrDispatchedUnknown) || errors.Is(err, herdr.ErrNotStarted) {
				t.Fatalf("error = %v, want started subprocess boundary", err)
			}
			d := &Dispatcher{logger: testLogger()}
			result := d.failErr("request-1", action, "pane-1", err)
			if result.OK || result.Phase != wantPhase || result.Error != wantMessage {
				t.Fatalf("result = %+v, want phase %q and message %q", result, wantPhase, wantMessage)
			}
			data, _ := result.Data.(map[string]any)
			if action == "submit_prompt" {
				if data["code"] != "agent_not_ready" || data["dispatched_unknown"] == true {
					t.Fatalf("data = %+v, want prompt refusal without uncertain dispatch", data)
				}
				partial := d.failErr("request-2", action, "pane-1", partiallyApplied("earlier input was delivered", err))
				partialData, _ := partial.Data.(map[string]any)
				if partial.Phase != "dispatched_unknown" || partialData["dispatched_unknown"] != true {
					t.Fatalf("result = %+v, want partial input to outrank prompt refusal", partial)
				}
				return
			}
			if data["dispatched_unknown"] != true || data["code"] != nil {
				t.Fatalf("data = %+v, want startup retry guard rather than prompt refusal", data)
			}
			if count := fake.count("StartAgent"); count != 1 {
				t.Fatalf("StartAgent calls = %d, want one dispatch without retry", count)
			}
		})
	}
}
