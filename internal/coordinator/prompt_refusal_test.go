package coordinator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func TestAgentNotReadyOutcomeDependsOnOperation(t *testing.T) {
	for _, action := range []string{"submit_prompt", "agent_start"} {
		t.Run(action, func(t *testing.T) {
			dir := t.TempDir()
			record := filepath.Join(dir, "calls")
			t.Setenv("HERDR_TEST_ARGS", record)
			bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
				"printf '%s\\n' \"$*\" >> \"$HERDR_TEST_ARGS\"\n"+
				"if [ \"$1 $2\" = \"agent start\" ]; then\n"+
				"  printf '%s' '{\"error\":{\"code\":\"agent_not_ready\",\"message\":\"agent reviewer is blocked during startup and is not ready for prompts\"}}' >&2\n"+
				"  exit 1\n"+
				"fi\n"+
				"if [ \"$1 $2\" = \"agent prompt\" ]; then\n"+
				"  printf '%s' '{\"error\":{\"code\":\"agent_not_ready\",\"message\":\"agent pane-1 is not an active named agent\"}}' >&2\n"+
				"  exit 1\n"+
				"fi\n")
			client := herdr.NewClient(bin, filepath.Join(dir, "herdr.sock"))
			t.Cleanup(func() { _ = client.Close() })
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var err error
			wantPhase := "not_started"
			wantMessage := "Agent is not ready to receive prompts; review the pane before retrying"
			if action == "agent_start" {
				lifecycle := &Lifecycle{herdr: client}
				err = lifecycle.startKindAgent(ctx, "claude", "reviewer", "pane-1")
				wantPhase = "dispatched_unknown"
				wantMessage = "Command may have executed; review the agent before retrying"
			} else {
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
			} else if data["dispatched_unknown"] != true || data["code"] != nil {
				t.Fatalf("data = %+v, want startup retry guard rather than prompt refusal", data)
			}
			calls, readErr := os.ReadFile(record)
			if readErr != nil {
				t.Fatalf("read Herdr invocations: %v", readErr)
			}
			if count := strings.Count(string(calls), "\n"); count != 1 {
				t.Fatalf("Herdr invocations = %q, want one dispatch without fallback or retry", calls)
			}
		})
	}
}
