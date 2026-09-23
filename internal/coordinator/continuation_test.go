package coordinator

import (
	"context"
	"testing"
	"time"
)

func TestContinuationReleasesCapacityAndKeepsLedgerPending(t *testing.T) {
	s := NewScheduler(2, testLogger())
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		_ = s.Close(context.Background())
	})
	entered := make(chan struct{})
	blockerDone := make(chan struct{})
	options := func(id string, pane string) ScheduleOptions {
		return ScheduleOptions{Command: Command{RequestID: id, Kind: CommandPrompt, PaneID: pane, Deadline: time.Now().Add(5 * time.Second)}}
	}
	go func() {
		defer close(blockerDone)
		_, _ = s.Execute(context.Background(), options("blocker", "pane"), EffectFunc(func(context.Context, WorkerToken) EffectResult {
			close(entered)
			<-release
			return EffectResult{Result: completed("blocker", "submit_prompt", "pane", nil)}
		}))
	}()
	<-entered
	launched := make(chan struct{})
	launchOptions := options("launch", "")
	launchOptions.RelayLevel, launchOptions.LedgerKey, launchOptions.PayloadHash = true, "launch", "payload"
	results := make(chan *CommandResult, 2)
	runner := EffectFunc(func(context.Context, WorkerToken) EffectResult {
		close(launched)
		return EffectResult{Continuation: &EffectContinuation{PaneID: "pane", Deadline: time.Now().Add(time.Second), Runner: EffectFunc(func(context.Context, WorkerToken) EffectResult {
			return EffectResult{Result: completed("prompt", "submit_prompt", "pane", nil)}
		}), Finalize: func(result *CommandResult, err error) *CommandResult {
			if err != nil || result == nil || !result.OK {
				return &CommandResult{OK: true, Phase: "completed_with_warning"}
			}
			return completed("launch", "agent_start", "pane", nil)
		}}}
	})
	go func() { result, _ := s.Execute(context.Background(), launchOptions, runner); results <- result }()
	<-launched
	go func() { result, _ := s.Execute(context.Background(), launchOptions, runner); results <- result }()
	other, err := s.Execute(context.Background(), options("other", "other-pane"), EffectFunc(func(context.Context, WorkerToken) EffectResult {
		return EffectResult{Result: completed("other", "submit_prompt", "other-pane", nil)}
	}))
	if err != nil || !other.OK {
		t.Fatalf("unrelated pane result = %+v, %v", other, err)
	}
	select {
	case result := <-results:
		t.Fatalf("early completion = %+v", result)
	default:
	}
	close(release)
	<-blockerDone
	for range 2 {
		result := <-results
		if result == nil || result.Phase != "completed" {
			t.Fatalf("result = %+v", result)
		}
	}
}

func TestContinuationInvalidationIsCachedWarning(t *testing.T) {
	for _, mode := range []string{"expired", "replaced", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			s := NewScheduler(2, testLogger())
			release := make(chan struct{})
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
				_ = s.Close(context.Background())
			})
			entered := make(chan struct{})
			deadline := time.Now().Add(5 * time.Second)
			go func() {
				_, _ = s.Execute(context.Background(), ScheduleOptions{Command: Command{PaneID: "pane", Deadline: deadline}}, EffectFunc(func(context.Context, WorkerToken) EffectResult {
					close(entered)
					<-release
					return EffectResult{Result: completed("blocker", "prompt", "pane", nil)}
				}))
			}()
			<-entered
			ready := make(chan struct{})
			options := ScheduleOptions{Command: Command{RequestID: "launch", Kind: CommandStart, Deadline: deadline}, RelayLevel: true, LedgerKey: "launch", PayloadHash: "payload"}
			results := make(chan *CommandResult, 1)
			go func() {
				result, _ := s.Execute(context.Background(), options, EffectFunc(func(context.Context, WorkerToken) EffectResult {
					promptDeadline := deadline
					if mode == "expired" {
						promptDeadline = time.Now().Add(-time.Second)
					}
					close(ready)
					return EffectResult{Continuation: &EffectContinuation{PaneID: "pane", Deadline: promptDeadline, Runner: EffectFunc(func(context.Context, WorkerToken) EffectResult {
						t.Error("invalidated prompt ran")
						return EffectResult{}
					}), Finalize: func(*CommandResult, error) *CommandResult {
						return &CommandResult{OK: true, Phase: "completed_with_warning"}
					}}}
				}))
				results <- result
			}()
			<-ready
			if mode == "replaced" {
				s.ApplyTopology(map[string]bool{"pane": true}, map[string]uint64{"pane": 1})
			}
			if mode == "shutdown" {
				s.CancelInflight()
			}
			select {
			case result := <-results:
				if result == nil || result.Phase != "completed_with_warning" {
					t.Fatalf("result = %+v", result)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("continuation did not finish")
			}
			if mode != "shutdown" {
				result, found, err := s.ReplayLedger("launch", "payload")
				if err != nil || !found || result == nil || result.Phase != "completed_with_warning" {
					t.Fatalf("replay = %+v, %v, %v", result, found, err)
				}
			}
			close(release)
		})
	}
}
