package coordinator

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
)

func TestQueuedPromptRejectsObservedNativeSessionChange(t *testing.T) {
	for _, replacement := range []string{"session-b", "", "revoked"} {
		t.Run(replacement, func(t *testing.T) {
			client := newFakeBackend()
			state := NewState(testLogger())
			state.CommitInventory([]*AgentState{{PaneID: "pane", TerminalID: "terminal", SessionID: "session-a", Status: "idle"}}, 0)
			d := newTestDispatcher(t, client, state)
			release, entered := make(chan struct{}), make(chan struct{})
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
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
				results <- d.HandleAdmitted(ctx, protocol.Inbound{Type: "submit_prompt", RequestID: "stale", PaneID: "pane", Text: "hello"}, func() { close(admitted) })
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
			if calls := client.recorded(); len(calls) != 0 {
				t.Fatalf("unexpected Herdr calls: %+v", calls)
			}
		})
	}
}
