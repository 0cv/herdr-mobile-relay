package coordinator

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/protocol"
)

func generationResult(t *testing.T, ctx context.Context, results <-chan *CommandResult) *CommandResult {
	t.Helper()
	select {
	case result := <-results:
		if result == nil {
			t.Fatal("command returned no result")
		}
		return result
	case <-ctx.Done():
		t.Fatal("command did not finish: ", ctx.Err())
		return nil
	}
}

func mutationQueuedBehindStop(t *testing.T, d *Dispatcher, client *fakeBackend, message protocol.Inbound) *CommandResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	client.setHook("StopPane", func(ctx context.Context, _ fakeCall) error {
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	stopped := make(chan *CommandResult, 1)
	go func() {
		stopped <- d.Handle(ctx, protocol.Inbound{
			Type:      "agent_stop",
			RequestID: "stop",
			PaneID:    "pane-1",
		})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("stop did not reach backend: ", ctx.Err())
	}

	admitted := make(chan struct{})
	results := make(chan *CommandResult, 1)
	go func() {
		results <- d.HandleAdmitted(ctx, message, func() { close(admitted) })
	}()
	select {
	case <-admitted:
	case <-ctx.Done():
		t.Fatal("mutation was not admitted: ", ctx.Err())
	}
	fence, err := d.scheduler.Execute(ctx, ScheduleOptions{RelayLevel: true, Command: Command{
		ID: d.scheduler.NextCommandID(), RequestID: "fence", ReceivedAt: time.Now(), Deadline: time.Now().Add(time.Second),
	}}, EffectFunc(func(context.Context, WorkerToken) EffectResult {
		return EffectResult{Result: completed("fence", "fence", "", nil)}
	}))
	if err != nil || fence == nil || !fence.OK {
		t.Fatalf("scheduler fence: result=%+v err=%v", fence, err)
	}
	select {
	case result := <-results:
		t.Fatalf("mutation did not wait for stop: %+v", result)
	default:
	}

	close(release)
	if result := generationResult(t, ctx, stopped); !result.OK {
		t.Fatalf("stop failed: %+v", result)
	}
	return generationResult(t, ctx, results)
}

// §9.5: a mutation that captured generation N must abort — without sending any
// input — if a Clear/Stop advances the generation while it was queued.
func TestMutationAbortsWhenGenerationAdvancesWhileQueued(t *testing.T) {
	client := newFakeBackend()
	d := newTestDispatcher(t, client, NewState(testLogger()))

	d.state.CommitInventory([]*AgentState{{PaneID: "pane-1", Status: "working"}}, d.state.RevisionCounter())
	res := mutationQueuedBehindStop(t, d, client, protocol.Inbound{
		Type: "submit_prompt", RequestID: "p", PaneID: "pane-1", Text: "hi",
	})
	if res.OK || res.Error != "pane session was replaced" {
		t.Fatalf("prompt dispatched after generation advanced: ok=%v phase=%q err=%q "+
			"(§9.5: input must not land on a replaced session)", res.OK, res.Phase, res.Error)
	}
	want := []fakeCall{{Method: "StopPane", Args: []any{"pane-1"}}}
	if got := client.recorded(); !reflect.DeepEqual(got, want) {
		t.Fatalf("prompt sent input to herdr despite a stale generation: calls = %+v", got)
	}
}

// §9.5 / §9.6: a stale-generation abort must release the approval ledger claim,
// so a fresh attempt (against the new session) can dispatch — the abort is a
// definite non-dispatch, not an uncertain one.
func TestApprovalLedgerReleasedOnStaleGeneration(t *testing.T) {
	client := newFakeBackend()
	client.contents["pane-1"] = approvalPane
	d := newTestDispatcher(t, client, NewState(testLogger()))
	commitApproval(d.state, "pane-1")
	eventID := blockedEventID(t, d, "pane-1")
	fingerprint, choice := approvalIdentity(t, d, "pane-1", 0)

	first := mutationQueuedBehindStop(t, d, client, protocol.Inbound{
		Type: "respond", RequestID: "a", PaneID: "pane-1", EventID: eventID,
		ApprovalFingerprint: fingerprint, Choice: choice, Index: intPtr(0), Total: intPtr(2),
	})
	if first.OK || first.Error != "pane session was replaced" {
		t.Fatalf("precondition: first approval err=%q, want stale-generation abort", first.Error)
	}

	// The claim must have been released: a retry can now dispatch.
	second := d.Handle(context.Background(), protocol.Inbound{
		Type:                "respond",
		RequestID:           "b",
		PaneID:              "pane-1",
		EventID:             eventID,
		ApprovalFingerprint: fingerprint,
		Choice:              choice,
		Index:               intPtr(0),
		Total:               intPtr(2),
	})
	if !second.OK {
		t.Fatalf("retry after stale-generation abort was blocked: phase=%q err=%q (ledger not released)", second.Phase, second.Error)
	}
	if sends := client.callsTo("SendKeys"); len(sends) != 1 {
		t.Fatalf("want exactly one approval dispatch after release, got: %+v", client.recorded())
	}
}

func TestSchedulerDoesNotRunMutationQueuedBeforeGenerationBump(t *testing.T) {
	scheduler := newTestScheduler(t, 1)

	started := make(chan struct{})
	release := make(chan struct{})
	clearResult := make(chan *CommandResult, 1)
	go func() {
		now := time.Now()
		result, _ := scheduler.Execute(context.Background(), ScheduleOptions{Command: Command{
			ID: scheduler.NextCommandID(), RequestID: "clear", ReceivedAt: now,
			Deadline: now.Add(time.Second), Action: "agent_clear", PaneID: "pane-1",
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
			Deadline: now.Add(time.Second), Action: "submit_prompt", PaneID: "pane-1",
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
	if result == nil || result.OK || result.Error != "pane session was replaced" || result.Action != "submit_prompt" {
		t.Fatalf("queued prompt result = %+v, want stale submit_prompt failure", result)
	}
	select {
	case <-promptRan:
		t.Fatal("queued prompt effect ran after generation bump")
	default:
	}
}

func TestQueuedMutationDoesNotReachReplacementPane(t *testing.T) {
	scheduler := newTestScheduler(t, 1)
	if !scheduler.ApplyTopology(
		map[string]bool{"pane-1": true},
		map[string]int64{"pane-1": 0},
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
			Deadline: now.Add(time.Second), Action: "submit_prompt", PaneID: "pane-1",
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
			Deadline: now.Add(time.Second), Action: "submit_prompt", PaneID: "pane-1",
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
		map[string]int64{"pane-1": 2},
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
		Deadline: now.Add(time.Second), Action: "submit_prompt", PaneID: "pane-1",
	}, PaneGeneration: 2}, EffectFunc(func(context.Context, WorkerToken) EffectResult {
		freshRan = true
		return EffectResult{Result: completed("fresh", "prompt", "pane-1", nil)}
	}))
	if err != nil || result == nil || !result.OK || !freshRan {
		t.Fatalf("fresh replacement prompt: result=%+v err=%v ran=%v", result, err, freshRan)
	}
}

func TestCommandReceivedWhilePaneAbsentDoesNotReachReplacement(t *testing.T) {
	f, old := newClearFixture(t, launchFixtureOptions{})
	d := f.dispatcher
	d.state.CommitInventory(nil, d.state.RevisionCounter())
	if got := d.Handle(f.ctx, protocol.Inbound{
		Type:   "submit_prompt",
		PaneID: old.PaneID,
		Text:   "must not cross sessions",
	}); got.OK || got.Error != ErrPaneReplaced.Error() {
		t.Fatalf("absent prompt result = %+v, want replaced-session failure", got)
	}

	client := f.client
	entered, release := make(chan struct{}), make(chan struct{})
	var blocked atomic.Bool
	client.setHook("CapabilityStatus", func(context.Context, fakeCall) error {
		if blocked.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
		return nil
	})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	results := make(chan *CommandResult, 1)
	go func() {
		results <- d.Handle(f.ctx, clearMessage())
	}()
	select {
	case <-entered:
	case <-f.ctx.Done():
		t.Fatal("clear did not reach backend: ", f.ctx.Err())
	}
	d.state.CommitInventory([]*AgentState{{
		PaneID: old.PaneID, RawPaneID: old.PaneID, TerminalID: "terminal-replacement",
		TabID: old.TabID, WorkspaceID: old.WorkspaceID, Agent: "claude", Status: "working", Cwd: f.cwd,
	}}, d.state.RevisionCounter())
	close(release)

	if got := generationResult(t, f.ctx, results); got.OK || got.Error != ErrPaneReplaced.Error() {
		t.Fatalf("absence-window clear result = %+v, want replaced-session failure", got)
	}
	if calls := client.recorded(); len(calls) != 0 {
		t.Fatalf("absence-window command reached replacement pane: %+v", calls)
	}
	if generation := d.state.Generation(old.PaneID); generation != 2 {
		t.Fatalf("replacement generation = %d, want distinct epoch 2", generation)
	}
}

func TestAbsentPaneMutationsDoNotInvokeHerdr(t *testing.T) {
	client := newFakeBackend()
	d := newTestDispatcher(t, client, NewState(testLogger()))

	messages := []protocol.Inbound{
		{Type: "send_keys", RequestID: "keys", PaneID: "missing", Keys: []string{"Enter"}},
		{Type: "send_input", RequestID: "text", PaneID: "missing", Text: "text"},
		{Type: "agent_stop", RequestID: "stop", PaneID: "missing"},
		{Type: "agent_rename", RequestID: "rename", PaneID: "missing", Name: "new-name"},
		{Type: "agent_clear", RequestID: "clear", PaneID: "missing"},
		{Type: "acknowledge_pane", RequestID: "ack", PaneID: "missing"},
		{Type: "respond", RequestID: "approval", PaneID: "missing", EventID: "old-event", Index: intPtr(0)},
		{Type: "answer_question", RequestID: "question", PaneID: "missing", InteractionID: "old-question"},
	}
	for _, message := range messages {
		if result := d.Handle(context.Background(), message); result.OK {
			t.Errorf("%s unexpectedly succeeded for absent pane", message.Type)
		}
	}
	if calls := client.recorded(); len(calls) != 0 {
		t.Fatalf("absent-pane mutation invoked Herdr: %+v", calls)
	}
}

func TestSchedulerInvalidatesSameIDReplacementGeneration(t *testing.T) {
	scheduler := newTestScheduler(t, 1)
	if !scheduler.ApplyTopology(
		map[string]bool{"pane-1": true},
		map[string]int64{"pane-1": 0},
	) {
		t.Fatal("initial topology was not applied")
	}

	started := make(chan struct{})
	release := make(chan struct{})
	go func() {
		now := time.Now()
		_, _ = scheduler.Execute(context.Background(), ScheduleOptions{Command: Command{
			ID: scheduler.NextCommandID(), RequestID: "first", ReceivedAt: now,
			Deadline: now.Add(time.Second), Action: "submit_prompt", PaneID: "pane-1",
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
			Deadline: now.Add(time.Second), Action: "submit_prompt", PaneID: "pane-1",
		}}, EffectFunc(func(context.Context, WorkerToken) EffectResult {
			return EffectResult{Result: completed("queued", "prompt", "pane-1", nil)}
		}), func() { close(admitted) })
		resultCh <- result
	}()
	<-admitted

	if !scheduler.ApplyTopology(
		map[string]bool{"pane-1": true},
		map[string]int64{"pane-1": 1},
	) {
		t.Fatal("replacement generation was not applied")
	}
	if result := <-resultCh; result == nil || result.OK || result.Error != "pane session was replaced" {
		t.Fatalf("same-ID replacement queued result = %+v", result)
	}
	close(release)
}

func TestSchedulerRejectsOperationAdmittedBeforeReplacementTopology(t *testing.T) {
	scheduler := newTestScheduler(t, 1)

	if !scheduler.ApplyTopology(
		map[string]bool{"pane-1": true},
		map[string]int64{"pane-1": 1},
	) {
		t.Fatal("replacement topology was not applied")
	}

	now := time.Now()
	ran := false
	result, err := scheduler.Execute(context.Background(), ScheduleOptions{
		Command: Command{
			ID: scheduler.NextCommandID(), RequestID: "stale-admission",
			ReceivedAt: now, Deadline: now.Add(time.Second),
			Action: "submit_prompt", PaneID: "pane-1",
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
