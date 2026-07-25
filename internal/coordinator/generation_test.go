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
	d := NewDispatcher(herdr.NewClient(recordingHerdr(t, dir, record, approvalPane), filepath.Join(dir, "sock")), NewState(testLogger()), nil, testLogger())
	commitApproval(d.state, "pane-1")
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

func TestQueuedMutationDoesNotReachReplacementPane(t *testing.T) {
	scheduler := NewScheduler(1, testLogger())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := scheduler.Close(ctx); err != nil {
			t.Errorf("close scheduler: %v", err)
		}
	})
	if !scheduler.ApplyTopology(
		map[string]bool{"pane-1": true},
		map[string]uint64{"pane-1": 0},
	) {
		t.Fatal("initial topology was not applied")
	}

	started := make(chan struct{})
	release := make(chan struct{})
	firstResult := make(chan *CommandResult, 1)
	go func() {
		now := time.Now()
		result, _ := scheduler.Execute(context.Background(), ScheduleOptions{Command: Command{
			ID: scheduler.NextCommandID(), RequestID: "first", ReceivedAt: now,
			Deadline: now.Add(time.Second), Kind: CommandPrompt, PaneID: "pane-1",
		}}, EffectFunc(func(context.Context, WorkerToken) EffectResult {
			close(started)
			<-release
			return EffectResult{Result: completed("first", "prompt", "pane-1", nil)}
		}))
		firstResult <- result
	}()
	<-started

	admitted := make(chan struct{})
	queuedResult := make(chan *CommandResult, 1)
	queuedRan := make(chan struct{}, 1)
	go func() {
		now := time.Now()
		result, _ := scheduler.ExecuteAdmitted(context.Background(), ScheduleOptions{Command: Command{
			ID: scheduler.NextCommandID(), RequestID: "queued", ReceivedAt: now,
			Deadline: now.Add(time.Second), Kind: CommandPrompt, PaneID: "pane-1",
		}}, EffectFunc(func(context.Context, WorkerToken) EffectResult {
			queuedRan <- struct{}{}
			return EffectResult{Result: completed("queued", "prompt", "pane-1", nil)}
		}), func() { close(admitted) })
		queuedResult <- result
	}()
	<-admitted

	if !scheduler.ApplyTopology(map[string]bool{}) {
		t.Fatal("pane removal topology was not applied")
	}
	if !scheduler.ApplyTopology(
		map[string]bool{"pane-1": true},
		map[string]uint64{"pane-1": 2},
	) {
		t.Fatal("replacement topology was not applied")
	}
	if result := <-queuedResult; result == nil || result.OK || result.Error != "pane session was replaced" {
		t.Fatalf("queued prompt result = %+v, want stale generation failure", result)
	}
	select {
	case <-queuedRan:
		t.Fatal("old-generation queued prompt ran against the replacement pane")
	default:
	}

	close(release)
	if result := <-firstResult; result == nil || result.OK || result.Phase != "dispatched_unknown" {
		t.Fatalf("in-flight prompt result = %+v, want dispatched-unknown result", result)
	}

	now := time.Now()
	freshRan := false
	result, err := scheduler.Execute(context.Background(), ScheduleOptions{Command: Command{
		ID: scheduler.NextCommandID(), RequestID: "fresh", ReceivedAt: now,
		Deadline: now.Add(time.Second), Kind: CommandPrompt, PaneID: "pane-1",
	}, PaneGeneration: 2}, EffectFunc(func(context.Context, WorkerToken) EffectResult {
		freshRan = true
		return EffectResult{Result: completed("fresh", "prompt", "pane-1", nil)}
	}))
	if err != nil || result == nil || !result.OK || !freshRan {
		t.Fatalf("fresh replacement prompt: result=%+v err=%v ran=%v", result, err, freshRan)
	}
}

func TestCommandReceivedWhilePaneAbsentDoesNotReachReplacement(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "sends.log")
	d := NewDispatcher(
		herdr.NewClient(recordingHerdr(t, dir, record, `{"ok":true}`), filepath.Join(dir, "sock")),
		NewState(testLogger()),
		nil,
		testLogger(),
	)
	t.Cleanup(func() {
		_ = d.Close(context.Background())
	})
	d.state.CommitInventory([]*AgentState{{
		PaneID: "pane-1", RawPaneID: "raw-old", TerminalID: "terminal-old", Status: "working",
	}}, d.state.RevisionCounter())
	d.state.CommitInventory(nil, d.state.RevisionCounter())

	// Hold the pre-scheduler test gate so the replacement appears after Handle
	// captured the pane's absence but before prompt scheduling.
	slot := d.paneSlot("pane-1")
	slot <- struct{}{}
	result := make(chan *CommandResult, 1)
	go func() {
		result <- d.Handle(context.Background(), map[string]any{
			"action": "submit_prompt", "request_id": "absent-prompt",
			"pane_id": "pane-1", "text": "must not cross sessions",
		})
	}()
	time.Sleep(20 * time.Millisecond)
	d.state.CommitInventory([]*AgentState{{
		PaneID: "pane-1", RawPaneID: "raw-new", TerminalID: "terminal-new", Status: "working",
	}}, d.state.RevisionCounter())
	<-slot

	if got := <-result; got.OK || got.Error != ErrPaneReplaced.Error() {
		t.Fatalf("absence-window prompt result = %+v, want replaced-session failure", got)
	}
	if data, _ := os.ReadFile(record); len(data) != 0 {
		t.Fatalf("absence-window prompt reached replacement pane:\n%s", data)
	}
	if generation := d.state.Generation("pane-1"); generation != 2 {
		t.Fatalf("replacement generation = %d, want distinct epoch 2", generation)
	}
}

func TestAbsentPaneMutationsDoNotInvokeHerdr(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "sends.log")
	d := NewDispatcher(
		herdr.NewClient(recordingHerdr(t, dir, record, `{"ok":true}`), filepath.Join(dir, "sock")),
		NewState(testLogger()),
		nil,
		testLogger(),
	)
	t.Cleanup(func() {
		_ = d.Close(context.Background())
	})

	messages := []map[string]any{
		{"action": "send_keys", "request_id": "keys", "pane_id": "missing", "keys": []any{"Enter"}},
		{"action": "send_text", "request_id": "text", "pane_id": "missing", "text": "text"},
		{"action": "agent_stop", "request_id": "stop", "pane_id": "missing"},
		{"action": "agent_rename", "request_id": "rename", "pane_id": "missing", "name": "new-name"},
		{"action": "agent_clear", "request_id": "clear", "pane_id": "missing"},
		{"action": "acknowledge_pane", "request_id": "ack", "pane_id": "missing"},
		{"action": "respond", "request_id": "approval", "pane_id": "missing", "event_id": "old-event", "index": float64(0)},
		{"action": "answer_question", "request_id": "question", "pane_id": "missing", "interaction_id": "old-question", "selected": []any{float64(0)}},
	}
	for _, message := range messages {
		if result := d.Handle(context.Background(), message); result.OK {
			t.Errorf("%s unexpectedly succeeded for absent pane", message["action"])
		}
	}
	if data, _ := os.ReadFile(record); len(data) != 0 {
		t.Fatalf("absent-pane mutation invoked Herdr:\n%s", data)
	}
}

func TestSchedulerInvalidatesSameIDReplacementGeneration(t *testing.T) {
	scheduler := NewScheduler(1, testLogger())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := scheduler.Close(ctx); err != nil {
			t.Errorf("close scheduler: %v", err)
		}
	})
	if !scheduler.ApplyTopology(
		map[string]bool{"pane-1": true},
		map[string]uint64{"pane-1": 0},
	) {
		t.Fatal("initial topology was not applied")
	}

	started := make(chan struct{})
	release := make(chan struct{})
	go func() {
		now := time.Now()
		_, _ = scheduler.Execute(context.Background(), ScheduleOptions{Command: Command{
			ID: scheduler.NextCommandID(), RequestID: "first", ReceivedAt: now,
			Deadline: now.Add(time.Second), Kind: CommandPrompt, PaneID: "pane-1",
		}}, EffectFunc(func(context.Context, WorkerToken) EffectResult {
			close(started)
			<-release
			return EffectResult{Result: completed("first", "prompt", "pane-1", nil)}
		}))
	}()
	<-started

	admitted := make(chan struct{})
	resultCh := make(chan *CommandResult, 1)
	go func() {
		now := time.Now()
		result, _ := scheduler.ExecuteAdmitted(context.Background(), ScheduleOptions{Command: Command{
			ID: scheduler.NextCommandID(), RequestID: "queued", ReceivedAt: now,
			Deadline: now.Add(time.Second), Kind: CommandPrompt, PaneID: "pane-1",
		}}, EffectFunc(func(context.Context, WorkerToken) EffectResult {
			return EffectResult{Result: completed("queued", "prompt", "pane-1", nil)}
		}), func() { close(admitted) })
		resultCh <- result
	}()
	<-admitted

	if !scheduler.ApplyTopology(
		map[string]bool{"pane-1": true},
		map[string]uint64{"pane-1": 1},
	) {
		t.Fatal("replacement generation was not applied")
	}
	if result := <-resultCh; result == nil || result.OK || result.Error != "pane session was replaced" {
		t.Fatalf("same-ID replacement queued result = %+v", result)
	}
	close(release)
}

func TestSchedulerRejectsOperationAdmittedBeforeReplacementTopology(t *testing.T) {
	scheduler := NewScheduler(1, testLogger())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := scheduler.Close(ctx); err != nil {
			t.Errorf("close scheduler: %v", err)
		}
	})

	if !scheduler.ApplyTopology(
		map[string]bool{"pane-1": true},
		map[string]uint64{"pane-1": 1},
	) {
		t.Fatal("replacement topology was not applied")
	}

	now := time.Now()
	ran := false
	result, err := scheduler.Execute(context.Background(), ScheduleOptions{
		Command: Command{
			ID: scheduler.NextCommandID(), RequestID: "stale-admission",
			ReceivedAt: now, Deadline: now.Add(time.Second),
			Kind: CommandPrompt, PaneID: "pane-1",
		},
		// The dispatcher captured generation zero before replacement, but its
		// scheduler ingress was handled after topology advanced to one.
		PaneGeneration: 0,
	}, EffectFunc(func(context.Context, WorkerToken) EffectResult {
		ran = true
		return EffectResult{Result: completed("stale-admission", "prompt", "pane-1", nil)}
	}))
	if err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Fatal("old-generation operation ran against the replacement pane")
	}
	if result == nil || result.OK || result.Error != ErrPaneReplaced.Error() {
		t.Fatalf("stale admission result = %+v, want replaced-session failure", result)
	}
}
