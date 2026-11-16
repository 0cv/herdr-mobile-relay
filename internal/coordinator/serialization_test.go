package coordinator

// Regression tests for the coordinator's per-pane FIFO serialization.

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/protocol"
)

// serializationRecorder records start and end events of fake Herdr calls and holds the first call until released.
type serializationRecorder struct {
	mu          sync.Mutex
	events      []string
	entered     chan struct{}
	release     chan struct{}
	enteredOnce sync.Once
	releaseOnce sync.Once
}

func newSerializationRecorder() *serializationRecorder {
	return &serializationRecorder{entered: make(chan struct{}), release: make(chan struct{})}
}

func (r *serializationRecorder) record(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *serializationRecorder) trace() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}

func (r *serializationRecorder) unblock() {
	r.releaseOnce.Do(func() { close(r.release) })
}

func (r *serializationRecorder) hook(label string, hold bool) fakeHook {
	return func(ctx context.Context, _ fakeCall) error {
		r.record("start " + label)
		if hold {
			r.enteredOnce.Do(func() { close(r.entered) })
			select {
			case <-r.release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		r.record("end " + label)
		return nil
	}
}

// assertSerializationTrace is shared by the regression and labelled overlay controls.
func assertSerializationTrace(t *testing.T, got []string, want ...string) {
	t.Helper()
	t.Logf("trace: %q", got)
	for i := 0; i < len(got) && i < len(want); i++ {
		if got[i] != want[i] {
			t.Fatalf("trace order mismatch at event %d: got %q, want %q", i+1, got[i], want[i])
		}
	}
	if len(got) != len(want) {
		t.Fatalf("trace length mismatch: got %q, want %q", got, want)
	}
}

// §10.2: both successful same-pane mutations must use the real pane FIFO.
func TestSamePaneCommandsSerialize(t *testing.T) {
	for _, first := range []string{"submit_prompt", "send_keys"} {
		t.Run(first+"_first", func(t *testing.T) {
			recorder := newSerializationRecorder()
			client := newFakeBackend()
			client.setHook("Prompt", recorder.hook("submit_prompt", first == "submit_prompt"))
			client.setHook("SendKeys", recorder.hook("send_keys", first == "send_keys"))
			state := NewState(testLogger())
			state.CommitInventory([]*AgentState{{
				PaneID: "pane-1", RawPaneID: "pane-1", TerminalID: "terminal-1",
				TabID: "tab-1", WorkspaceID: "workspace-1", Agent: "claude", Status: "idle",
			}}, state.RevisionCounter())
			d := newTestDispatcher(t, client, state)
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
			results := [2]chan *CommandResult{make(chan *CommandResult, 1), make(chan *CommandResult, 1)}
			var joins []chan struct{}
			t.Cleanup(func() {
				recorder.unblock()
				cancel()
				d.CancelInflight()
				cleanupCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
				defer stop()
				for _, done := range joins {
					select {
					case <-done:
					case <-cleanupCtx.Done():
						t.Error("caller cleanup: ", cleanupCtx.Err())
					}
				}
			})
			agent, ok := state.Agent("pane-1")
			generation, active := state.PaneSession("pane-1")
			if !ok || !active || !state.PaneSessionCurrent("pane-1", generation) ||
				agent.PaneID != "pane-1" || agent.RawPaneID != "pane-1" || agent.TerminalID != "terminal-1" ||
				agent.TabID != "tab-1" || agent.WorkspaceID != "workspace-1" || agent.Agent != "claude" || agent.Status != "idle" {
				t.Fatalf("invalid committed identity/session: %+v, %d, %v", agent, generation, active)
			}
			messages := []protocol.Inbound{
				{Type: "submit_prompt", RequestID: "a", PaneID: "pane-1", Text: "one"},
				{Type: "send_keys", RequestID: "b", PaneID: "pane-1", Keys: []string{"x"}},
			}
			events := []string{"submit_prompt", "send_keys"}
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
			select {
			case <-recorder.entered:
			case result := <-results[0]:
				t.Fatalf("first command returned before release: %+v", result)
			case <-ctx.Done():
				t.Fatal("first gate: ", ctx.Err())
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
			assertSerializationTrace(t, recorder.trace(), "start "+events[0])
			select {
			case result := <-results[0]:
				t.Fatalf("first command returned before release: %+v", result)
			default:
			}
			recorder.unblock()
			for i := range results {
				select {
				case result := <-results[i]:
					if result == nil || !result.OK || result.Phase != "completed" || result.Error != "" ||
						result.RequestID != messages[i].RequestID || result.Action != messages[i].Type || result.PaneID != "pane-1" {
						t.Fatalf("intended successful command %d: %+v", i, result)
					}
					t.Logf("successful command %d: %+v", i, result)
				case <-ctx.Done():
					t.Fatal("command join: ", ctx.Err())
				}
			}
			assertSerializationTrace(t, recorder.trace(), "start "+events[0], "end "+events[0], "start "+events[1], "end "+events[1])
		})
	}
}

// §10.2 / §16.10: Clear must serialize with other mutations on the same pane —
// a prompt must never be dispatched concurrently with a clear of that pane.
func TestClearSerializesWithPrompt(t *testing.T) {
	runClearSerialization(t)
}
