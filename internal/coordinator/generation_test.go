package coordinator

// Verification tests for lifecycle generations (§9.5). Unlike the red tests,
// these are expected to PASS — they lock the implemented behavior against
// regression. They drive the interleaving deterministically by holding the
// pane FIFO slot while a Clear/Stop bumps the generation.
// Helpers testLogger/recordingHerdr live in sibling *_test.go files.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

// §9.5: a mutation that captured generation N must abort — without sending any
// input — if a Clear/Stop advances the generation while it was queued.
func TestMutationAbortsWhenGenerationAdvancesWhileQueued(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "sends.log")
	d := NewDispatcher(herdr.NewClient(recordingHerdr(t, dir, record, `{"ok":true}`), filepath.Join(dir, "sock")), NewState(testLogger()), nil, testLogger())

	// Occupy the pane FIFO so the prompt captures the generation and then queues.
	slot := d.paneSlot("pane-1")
	slot <- struct{}{}

	resCh := make(chan *CommandResult, 1)
	go func() {
		resCh <- d.Handle(context.Background(), map[string]any{
			"action": "submit_prompt", "request_id": "p", "pane_id": "pane-1", "text": "hi",
		})
	}()

	time.Sleep(100 * time.Millisecond) // prompt is now parked on slot.Lock(), gen captured

	// A Clear commits while the prompt is queued.
	d.state.BumpGeneration("pane-1")
	<-slot

	res := <-resCh
	if res.OK || res.Error != "pane session was replaced" {
		t.Fatalf("prompt dispatched after generation advanced: ok=%v phase=%q err=%q "+
			"(§9.5: input must not land on a replaced session)", res.OK, res.Phase, res.Error)
	}
	if data, _ := os.ReadFile(record); strings.Contains(string(data), "send-text") {
		t.Fatalf("prompt sent input to herdr despite a stale generation:\n%s", data)
	}
}

// §9.5 / §9.6: a stale-generation abort must release the approval ledger claim,
// so a fresh attempt (against the new session) can dispatch — the abort is a
// definite non-dispatch, not an uncertain one.
func TestApprovalLedgerReleasedOnStaleGeneration(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "sends.log")
	d := NewDispatcher(herdr.NewClient(recordingHerdr(t, dir, record, `{"ok":true}`), filepath.Join(dir, "sock")), NewState(testLogger()), nil, testLogger())
	d.state.CommitInventory([]*AgentState{{PaneID: "pane-1", Status: "blocked"}}, d.state.RevisionCounter())
	eventID := blockedEventID(t, d, "pane-1")

	slot := d.paneSlot("pane-1")
	slot <- struct{}{}

	resCh := make(chan *CommandResult, 1)
	go func() {
		resCh <- d.Handle(context.Background(), map[string]any{
			"action": "respond", "request_id": "a", "pane_id": "pane-1", "event_id": eventID, "index": float64(0),
		})
	}()

	time.Sleep(100 * time.Millisecond)
	d.state.BumpGeneration("pane-1")
	<-slot

	if first := <-resCh; first.Error != "pane session was replaced" {
		t.Fatalf("precondition: first approval err=%q, want stale-generation abort", first.Error)
	}

	// The claim must have been released: a retry can now dispatch.
	second := d.Handle(context.Background(), map[string]any{
		"action": "respond", "request_id": "b", "pane_id": "pane-1", "event_id": eventID, "index": float64(0),
	})
	if !second.OK {
		t.Fatalf("retry after stale-generation abort was blocked: phase=%q err=%q (ledger not released)", second.Phase, second.Error)
	}
	if data, _ := os.ReadFile(record); strings.Count(string(data), "send-keys") != 1 {
		t.Fatalf("want exactly one approval dispatch after release, got:\n%s", data)
	}
}

func TestSchedulerDoesNotRunMutationQueuedBeforeGenerationBump(t *testing.T) {
	scheduler := NewScheduler(1, testLogger())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := scheduler.Close(ctx); err != nil {
			t.Errorf("close scheduler: %v", err)
		}
	})

	started := make(chan struct{})
	release := make(chan struct{})
	clearResult := make(chan *CommandResult, 1)
	go func() {
		now := time.Now()
		result, _ := scheduler.Execute(context.Background(), ScheduleOptions{Command: Command{
			ID: scheduler.NextCommandID(), RequestID: "clear", ReceivedAt: now,
			Deadline: now.Add(time.Second), Kind: CommandClear, PaneID: "pane-1",
		}}, EffectFunc(func(context.Context, WorkerToken) EffectResult {
			close(started)
			<-release
			return EffectResult{
				Result:         completed("clear", "agent_clear", "pane-1", nil),
				BumpGeneration: true,
			}
		}))
		clearResult <- result
	}()
	<-started

	admitted := make(chan struct{})
	promptRan := make(chan struct{}, 1)
	promptResult := make(chan *CommandResult, 1)
	go func() {
		now := time.Now()
		result, _ := scheduler.ExecuteAdmitted(context.Background(), ScheduleOptions{Command: Command{
			ID: scheduler.NextCommandID(), RequestID: "prompt", ReceivedAt: now,
			Deadline: now.Add(time.Second), Kind: CommandPrompt, PaneID: "pane-1",
		}}, EffectFunc(func(context.Context, WorkerToken) EffectResult {
			promptRan <- struct{}{}
			return EffectResult{Result: completed("prompt", "prompt", "pane-1", nil)}
		}), func() { close(admitted) })
		promptResult <- result
	}()
	<-admitted
	time.Sleep(20 * time.Millisecond)
	close(release)

	if result := <-clearResult; result == nil || !result.OK {
		t.Fatalf("clear result = %+v", result)
	}
	result := <-promptResult
	if result == nil || result.OK || result.Error != "pane session was replaced" {
		t.Fatalf("queued prompt result = %+v, want stale generation failure", result)
	}
	select {
	case <-promptRan:
		t.Fatal("queued prompt effect ran after generation bump")
	default:
	}
}
