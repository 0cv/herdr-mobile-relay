package coordinator

// Regression tests for the coordinator's per-pane FIFO serialization.
// Helpers writeScript/testLogger live in sibling *_test.go files.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

// assertSerializationTrace is shared by the regression and labelled overlay controls.
func assertSerializationTrace(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("trace: %q", data)
	gotLines, wantLines := strings.Split(string(data), "\n"), strings.Split(want, "\n")
	for i := 0; i < len(gotLines) && i < len(wantLines); i++ {
		if gotLines[i] != wantLines[i] {
			t.Fatalf("trace order mismatch at event %d: got %q, want %q", i+1, gotLines[i], wantLines[i])
		}
	}
	if string(data) != want {
		t.Fatalf("trace length mismatch: got %q, want %q", data, want)
	}
}

// §10.2: both successful same-pane mutations must use the real pane FIFO.
func TestSamePaneCommandsSerialize(t *testing.T) {
	for _, first := range []string{"submit_prompt", "send_keys"} {
		t.Run(first+"_first", func(t *testing.T) {
			dir := t.TempDir()
			trace, entered, release := filepath.Join(dir, "trace"), filepath.Join(dir, "entered"), filepath.Join(dir, "release")
			bin := writeScript(t, dir, "herdr", `#!/bin/sh
set -eu
[ "$#" -eq 4 ] && [ "$3" = pane-1 ] || exit 2
case "$1:$2:$4" in
 agent:prompt:one) label=submit_prompt ;;
 pane:send-keys:x) label=send_keys ;;
 *) exit 2 ;;
esac
printf 'start %s %s %s %s %s\n' "$label" "$1" "$2" "$3" "$4" >> '`+trace+`'
if [ "$label" = '`+first+`' ]; then
 mkdir '`+entered+`'
 while [ ! -f '`+release+`' ]; do /bin/sleep 0.01; done
fi
printf 'end %s %s %s %s %s\n' "$label" "$1" "$2" "$3" "$4" >> '`+trace+`'
`)
			client := herdr.NewClient(bin, filepath.Join(dir, "sock"))
			state := NewState(testLogger())
			state.CommitInventory([]*AgentState{{
				PaneID: "pane-1", RawPaneID: "pane-1", TerminalID: "terminal-1",
				TabID: "tab-1", WorkspaceID: "workspace-1", Agent: "claude", Status: "idle",
			}}, state.RevisionCounter())
			d := NewDispatcher(client, state, nil, testLogger())
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
			results := [2]chan *CommandResult{make(chan *CommandResult, 1), make(chan *CommandResult, 1)}
			var joins []chan struct{}
			unblock := func() {
				if err := os.WriteFile(release, nil, 0600); err != nil {
					t.Errorf("release: %v", err)
				}
			}
			t.Cleanup(func() {
				unblock()
				cancel()
				d.CancelInflight()
				cleanupCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
				defer stop()
				if err := d.Close(cleanupCtx); err != nil {
					t.Errorf("dispatcher cleanup: %v", err)
				}
				for _, done := range joins {
					select {
					case <-done:
					case <-cleanupCtx.Done():
						t.Error("caller cleanup: ", cleanupCtx.Err())
					}
				}
				if err := client.Close(); err != nil {
					t.Errorf("client cleanup: %v", err)
				}
			})
			agent, ok := state.Agent("pane-1")
			generation, active := state.PaneSession("pane-1")
			if !ok || !active || !state.PaneSessionCurrent("pane-1", uint64(generation)) ||
				agent.PaneID != "pane-1" || agent.RawPaneID != "pane-1" || agent.TerminalID != "terminal-1" ||
				agent.TabID != "tab-1" || agent.WorkspaceID != "workspace-1" || agent.Agent != "claude" || agent.Status != "idle" {
				t.Fatalf("invalid committed identity/session: %+v, %d, %v", agent, generation, active)
			}
			messages := []map[string]any{
				{"action": "submit_prompt", "request_id": "a", "pane_id": "pane-1", "text": "one"},
				{"action": "send_keys", "request_id": "b", "pane_id": "pane-1", "keys": []any{"x"}},
			}
			events := []string{"submit_prompt agent prompt pane-1 one\n", "send_keys pane send-keys pane-1 x\n"}
			if first == "send_keys" {
				messages[0], messages[1] = messages[1], messages[0]
				events[0], events[1] = events[1], events[0]
			}
			admitted := make(chan struct{})
			launch := func(i int) {
				done := make(chan struct{})
				joins = append(joins, done)
				go func() {
					defer close(done)
					if i == 0 {
						results[i] <- d.Handle(ctx, messages[i])
						return
					}
					results[i] <- d.HandleAdmitted(ctx, messages[i], func() { close(admitted) })
				}()
			}
			launch(0)
			for {
				select {
				case result := <-results[0]:
					t.Fatalf("first command returned before release: %+v", result)
				case <-ctx.Done():
					t.Fatal("first gate: ", ctx.Err())
				default:
				}
				info, err := os.Stat(entered)
				if err == nil {
					if !info.IsDir() {
						t.Fatal("invalid first gate: not a directory")
					}
					break
				}
				if !os.IsNotExist(err) {
					t.Fatal("first gate: ", err)
				}
				select {
				case result := <-results[0]:
					t.Fatalf("first command returned before release: %+v", result)
				case <-ctx.Done():
					t.Fatal("first gate: ", ctx.Err())
				case <-time.After(time.Millisecond):
				}
			}
			launch(1)
			select {
			case <-admitted:
			case result := <-results[0]:
				t.Fatalf("first command returned before release: %+v", result)
			case <-ctx.Done():
				t.Fatal("second admission: ", ctx.Err())
			}
			// Callback is only ingress send; this no-op fences the same owner FIFO.
			fence, err := d.scheduler.Execute(ctx, ScheduleOptions{RelayLevel: true, Command: Command{
				ID: d.scheduler.NextCommandID(), RequestID: "fence", ReceivedAt: time.Now(), Deadline: time.Now().Add(time.Second),
			}}, EffectFunc(func(context.Context, WorkerToken) EffectResult {
				return EffectResult{Result: completed("fence", "fence", "", nil)}
			}))
			if err != nil || fence == nil || !fence.OK || fence.Phase != "completed" || fence.Error != "" {
				t.Fatalf("owner fence failed: %+v, %v", fence, err)
			}
			metrics := d.scheduler.Metrics()
			if metrics.Dispatched != 2 || metrics.Completed != 1 || metrics.HerdrInUse != 1 ||
				metrics.RejectedNotStarted != 0 || metrics.ExpiredQueued != 0 {
				t.Fatalf("held owner accounting: %+v", metrics)
			}
			t.Logf("held owner accounting: %+v", metrics)
			assertSerializationTrace(t, trace, "start "+events[0])
			select {
			case result := <-results[0]:
				t.Fatalf("first command returned before release: %+v", result)
			default:
			}
			unblock()
			for i := range results {
				select {
				case result := <-results[i]:
					if result == nil || !result.OK || result.Phase != "completed" || result.Error != "" ||
						result.RequestID != messages[i]["request_id"] || result.Action != messages[i]["action"] || result.PaneID != "pane-1" {
						t.Fatalf("intended successful command %d: %+v", i, result)
					}
					t.Logf("successful command %d: %+v", i, result)
				case <-ctx.Done():
					t.Fatal("command join: ", ctx.Err())
				}
			}
			assertSerializationTrace(t, trace, "start "+events[0]+"end "+events[0]+"start "+events[1]+"end "+events[1])
		})
	}
}

// §10.2 / §16.10: Clear must serialize with other mutations on the same pane —
// a prompt must never be dispatched concurrently with a clear of that pane.
func TestClearSerializesWithPrompt(t *testing.T) {
	runClearSerialization(t)
}
