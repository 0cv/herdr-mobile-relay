package coordinator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func TestQueuedPromptRejectsObservedNativeSessionChange(t *testing.T) {
	for _, replacement := range []string{"session-b", "", "revoked"} {
		t.Run(replacement, func(t *testing.T) {
			dir := t.TempDir()
			record := filepath.Join(dir, "calls")
			state := NewState(testLogger())
			state.CommitInventory([]*AgentState{{PaneID: "pane", TerminalID: "terminal", SessionID: "session-a", Status: "idle"}}, 0)
			d := NewDispatcher(herdr.NewClient(recordingHerdr(t, dir, record, `{"result":{}}`), filepath.Join(dir, "sock")), state, nil, testLogger())
			release, entered := make(chan struct{}), make(chan struct{})
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
				_ = d.Close(context.Background())
			})
			go func() {
				_, _ = d.scheduler.Execute(context.Background(), ScheduleOptions{Command: Command{PaneID: "pane", Deadline: time.Now().Add(5 * time.Second)}}, EffectFunc(func(context.Context, WorkerToken) EffectResult {
					close(entered)
					<-release
					return EffectResult{Result: completed("block", "prompt", "pane", nil)}
				}))
			}()
			<-entered
			admitted, results := make(chan struct{}), make(chan *CommandResult, 1)
			var revoked atomic.Bool
			ctx := herdr.WithDispatchCheck(context.Background(), func() error {
				if revoked.Load() {
					return errors.New("credential revoked")
				}
				return nil
			})
			go func() {
				results <- d.HandleAdmitted(ctx, map[string]any{"action": "submit_prompt", "request_id": "stale", "pane_id": "pane", "text": "hello"}, func() { close(admitted) })
			}()
			<-admitted
			if replacement == "revoked" {
				revoked.Store(true)
			} else {
				state.CommitInventory([]*AgentState{{PaneID: "pane", TerminalID: "terminal", SessionID: replacement, Status: "idle"}}, state.Revision("pane"))
			}
			close(release)
			result := <-results
			if result.OK {
				t.Fatalf("stale prompt succeeded: %+v", result)
			}
			if data, err := os.ReadFile(record); err == nil {
				t.Fatalf("unexpected subprocess writes: %s", data)
			} else if !os.IsNotExist(err) {
				t.Fatal(err)
			}
		})
	}
}
