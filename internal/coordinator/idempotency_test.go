package coordinator

// Idempotency regressions beyond approvals: pre-pane Agent Start (§9.6/§16.9)
// and structured-question answers (§9.6).

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/question"
)

func keyCalls(client *fakeBackend, key string) int {
	count := 0
	for _, call := range client.callsTo("SendKeys") {
		if slices.Contains(call.Args[1].([]string), key) {
			count++
		}
	}
	return count
}

// §9.6 / §16.9: the relay-level pre-pane ledger shares one launch between
// concurrent requests with the same identity.
func TestDuplicateAgentStartCreatesOnePane(t *testing.T) {
	f := newLaunchFixture(t)
	results := make([]*CommandResult, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i] = f.dispatcher.Handle(f.ctx, protocol.Inbound{
				Type:      "agent_start",
				RequestID: "same-req",
				ProfileID: "claude",
				Name:      "proj",
				Cwd:       f.cwd,
			})
		}()
	}
	close(start)
	wg.Wait()
	for _, result := range results {
		f.assertLaunch(t, result)
	}
	f.assertCalls(t, 0)
}

func TestAgentStartRetryDoesNotResubmitInitialPrompt(t *testing.T) {
	f := newLaunchFixture(t)
	message := protocol.Inbound{
		Type:      "agent_start",
		RequestID: "same-req",
		ProfileID: "claude",
		Name:      "proj",
		Cwd:       f.cwd,
		Prompt:    "hello",
	}
	first := f.dispatcher.Handle(f.ctx, message)
	second := f.dispatcher.Handle(f.ctx, message)
	f.assertLaunch(t, first)
	f.assertLaunch(t, second)
	if !second.replayed {
		t.Fatal("duplicate launch did not replay the stored result")
	}
	f.assertCalls(t, 1)
}

func TestLedgerReplayReturnsConfirmationWatchPhase(t *testing.T) {
	scheduler := newTestScheduler(t, 1)

	const ledgerKey = "approval\x00pane-1\x00event-1"
	options := func() ScheduleOptions {
		now := time.Now()
		return ScheduleOptions{
			Command: Command{
				ID:         scheduler.NextCommandID(),
				RequestID:  "approve",
				ReceivedAt: now,
				Deadline:   now.Add(time.Second),
				Action:     "approval",
				PaneID:     "pane-1",
			},
			LedgerKey:   ledgerKey,
			PayloadHash: "choice-1-of-2",
		}
	}
	runs := 0
	runner := EffectFunc(func(context.Context, WorkerToken) EffectResult {
		runs++
		result := completed("approve", "approval", "pane-1", nil)
		result.Phase = "accepted"
		return EffectResult{Result: result}
	})

	first, err := scheduler.Execute(context.Background(), options(), runner)
	if err != nil || first == nil || first.Phase != "accepted" {
		t.Fatalf("first result = %+v, err = %v", first, err)
	}
	if !scheduler.UpdateLedgerPhase(ledgerKey, 0, "confirmed") {
		t.Fatal("confirmation phase was not applied")
	}

	replay, err := scheduler.Execute(context.Background(), options(), runner)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replay == nil || replay.Phase != "confirmed" || !replay.OK || !replay.replayed {
		t.Fatalf("replay = %+v, want confirmed stored phase", replay)
	}
	if runs != 1 {
		t.Fatalf("effect runs = %d, want 1", runs)
	}
	if scheduler.UpdateLedgerPhase(ledgerKey, 1, "unconfirmed") {
		t.Fatal("stale-generation phase update was applied")
	}
}

func TestAgentStopRetryDoesNotCloseOrBumpTwice(t *testing.T) {
	client := newFakeBackend()
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Status: "working"}}, state.RevisionCounter())
	d := newTestDispatcher(t, client, state)
	message := protocol.Inbound{
		Type: "agent_stop", RequestID: "stop-1", PaneID: "pane-1",
	}
	first := d.Handle(context.Background(), message)
	second := d.Handle(context.Background(), message)
	if !first.OK || !second.OK {
		t.Fatalf("stop results = %+v, %+v", first, second)
	}
	if closes := client.callsTo("StopPane"); len(closes) != 1 {
		t.Fatalf("StopPane calls = %d, want 1: %+v", len(closes), client.recorded())
	}
	if generation := state.Generation("pane-1"); generation != 1 {
		t.Fatalf("state generation = %d, want 1", generation)
	}
}

func TestTabRenameAcceptsNaturalLabel(t *testing.T) {
	client := newFakeBackend()
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{
		PaneID: "pane-1", TabID: "tab-1", Status: "working",
	}}, state.RevisionCounter())
	d := newTestDispatcher(t, client, state)

	result := d.Handle(context.Background(), protocol.Inbound{
		Type:      "agent_rename",
		RequestID: "rename-1",
		PaneID:    "pane-1",
		Name:      "123",
	})
	if !result.OK {
		t.Fatalf("numeric rename failed: %+v", result)
	}
	want := []fakeCall{{Method: "TabRename", Args: []any{"tab-1", "123"}}}
	if got := client.recorded(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %+v, want only %+v", got, want)
	}
}

func TestTabReorderUsesHerdrSocketAPI(t *testing.T) {
	client := newFakeBackend()
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{
		PaneID: "pane-1", TabID: "tab-1", Status: "working",
	}}, state.RevisionCounter())
	dispatcher := newTestDispatcher(t, client, state)
	result := dispatcher.Handle(context.Background(), protocol.Inbound{
		Type: "tab_reorder", RequestID: "move-1", PaneID: "pane-1", InsertIndex: intPtr(2),
	})
	if !result.OK {
		t.Fatalf("tab reorder failed: %+v", result)
	}
	want := []fakeCall{{Method: "TabMove", Args: []any{"tab-1", 2}}}
	if got := client.recorded(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %+v, want %+v", got, want)
	}
}

func TestApprovalRetryAfterStatusChangeReturnsStoredPhase(t *testing.T) {
	client := newFakeBackend()
	client.contents["pane-1"] = approvalPane
	state := NewState(testLogger())
	commitApproval(state, "pane-1")
	eventID := blockedEventID(t, &Dispatcher{state: state}, "pane-1")
	d := newTestDispatcher(t, client, state)
	fingerprint, choice := approvalIdentity(t, d, "pane-1", 0)
	updates := make(chan map[string]any, 1)
	d.SetBroadcast(func(message any) {
		if update, ok := message.(map[string]any); ok {
			updates <- update
		}
	})
	message := protocol.Inbound{
		Type:                "respond",
		RequestID:           "approval-1",
		PaneID:              "pane-1",
		EventID:             eventID,
		ApprovalFingerprint: fingerprint,
		Choice:              choice,
		Index:               intPtr(0),
		Total:               intPtr(2),
	}
	first := d.Handle(context.Background(), message)
	if !first.OK || first.Phase != "accepted" {
		t.Fatalf("first approval = %+v", first)
	}
	state.CommitEvent("pane-1", "idle", time.Now().UnixMilli())
	select {
	case update := <-updates:
		if update["phase"] != "confirmed" {
			t.Fatalf("watch update = %+v", update)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("confirmation watch did not complete")
	}
	replay := d.Handle(context.Background(), message)
	if !replay.OK || replay.Phase != "confirmed" {
		t.Fatalf("approval replay = %+v, want stored confirmed phase", replay)
	}
	if sends := client.callsTo("SendKeys"); len(sends) != 1 {
		t.Fatalf("approval sends = %d, want 1: %+v", len(sends), client.recorded())
	}
}

// §9.6: repeating answer_question with the same interaction_id must send the
// answer keys only once.
func TestDuplicateQuestionAnswerSendsOnce(t *testing.T) {
	questionView := "Which deployment target?\n❯ 1. Development\n2. Staging\n3. Type something.\n4. Chat about this\nEnter to select · ↑/↓ to navigate · Esc to cancel"
	interaction := question.Parse(questionView, "claude")
	if interaction == nil {
		t.Fatal("test question did not parse")
	}
	client := newFakeBackend()
	client.contents["pane-1"] = questionView

	d := newTestDispatcher(t, client, NewState(testLogger()))
	d.state.CommitInventory([]*AgentState{{PaneID: "pane-1", Agent: "claude", Status: "blocked"}}, d.state.RevisionCounter())

	answer := func() *CommandResult {
		return d.Handle(context.Background(), protocol.Inbound{
			Type:            "answer_question",
			RequestID:       "q",
			PaneID:          "pane-1",
			InteractionID:   interaction.ID,
			SelectedIndices: []int{0},
		})
	}

	answer() // first submission
	answer() // duplicate: retry or a second client on the same interaction

	conflict := d.Handle(context.Background(), protocol.Inbound{
		Type:            "answer_question",
		RequestID:       "q",
		PaneID:          "pane-1",
		InteractionID:   interaction.ID,
		SelectedIndices: []int{1},
	})
	if conflict.OK || !strings.Contains(conflict.Error, "different response") {
		t.Fatalf("same request with different answer = %+v, want conflict", conflict)
	}

	// Each answer confirms with a single Enter; a deduped answer sends it once.
	if enters := keyCalls(client, "Enter"); enters != 1 {
		t.Fatalf("duplicate question answer confirmed %d times, want 1: %+v", enters, client.recorded())
	}
}

func TestQuestionWatcherWaitsThroughTransientUnparseableFrames(t *testing.T) {
	tests := []struct {
		name          string
		initialView   string
		nextView      string
		action        string
		direction     string
		expectedPhase string
	}{
		{
			name:          "answer advances",
			initialView:   "Question 1/2 (2 unanswered)\nChoose the first value\n\n❯ 1. Alpha\n  2. Beta\n  3. None of the above\n\ntab to add notes | enter to submit answer | ←/→ to navigate questions",
			nextView:      "Question 2/2 (1 unanswered)\nChoose the second value\n\n❯ 1. Gamma\n  2. Delta\n  3. None of the above\n\ntab to add notes | enter to submit all | ←/→ to navigate questions",
			action:        "answer_question",
			expectedPhase: "advanced",
		},
		{
			name:          "previous navigates",
			initialView:   "Question 2/2 (1 unanswered)\nChoose the second value\n\n❯ 1. Gamma\n  2. Delta\n  3. None of the above\n\ntab to add notes | enter to submit all | ←/→ to navigate questions",
			nextView:      "Question 1/2 (2 unanswered)\nChoose the first value\n\n❯ 1. Alpha\n  2. Beta\n  3. None of the above\n\ntab to add notes | enter to submit answer | ←/→ to navigate questions",
			action:        "navigate_question",
			direction:     "previous",
			expectedPhase: "navigated",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := newFakeBackend()
			reads := 0
			client.setHook("ReadPane", func(context.Context, fakeCall) error {
				reads++
				view := test.nextView
				switch reads {
				case 1:
					view = test.initialView
				case 2:
					view = "redrawing question"
				}
				client.update(func(b *fakeBackend) { b.contents["pane-1"] = view })
				return nil
			})
			state := NewState(testLogger())
			state.CommitInventory([]*AgentState{{
				PaneID: "pane-1", Agent: "codex", Status: "blocked",
			}}, state.RevisionCounter())
			d := newTestDispatcher(t, client, state)
			updates := make(chan map[string]any, 8)
			d.SetBroadcast(func(message any) {
				if update, ok := message.(map[string]any); ok {
					updates <- update
				}
			})
			interaction := question.Parse(test.initialView, "codex")
			if interaction == nil {
				t.Fatal("initial question did not parse")
			}
			message := protocol.Inbound{
				Type:          test.action,
				RequestID:     "transition",
				PaneID:        "pane-1",
				InteractionID: interaction.ID,
			}
			if test.direction != "" {
				message.Direction = test.direction
			} else {
				message.SelectedIndices = []int{0}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			if accepted := d.Handle(ctx, message); !accepted.OK || accepted.Phase != "accepted" {
				t.Fatalf("accepted result = %+v", accepted)
			}
			for {
				select {
				case update := <-updates:
					if update["request_id"] != "transition" {
						continue
					}
					if update["phase"] == "confirmed" {
						t.Fatalf("transient frame was treated as final: %+v", update)
					}
					if update["phase"] == test.expectedPhase {
						return
					}
				case <-ctx.Done():
					t.Fatalf("no %s result", test.expectedPhase)
				}
			}
		})
	}
}

func TestAcknowledgeOnlyBroadcastsDisplayedStatusChanges(t *testing.T) {
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{
		PaneID: "pane-1", Agent: "claude", Status: "blocked",
	}}, state.RevisionCounter())
	d := newTestDispatcher(t, nil, state)
	updates := make(chan AgentUpdate, 2)
	d.SetBroadcast(func(message any) {
		if update, ok := message.(AgentUpdate); ok {
			updates <- update
		}
	})

	if result := d.handleAcknowledge("blocked", "pane-1"); !result.OK {
		t.Fatalf("blocked acknowledgement = %+v", result)
	}
	select {
	case update := <-updates:
		t.Fatalf("unchanged blocked status broadcast a sparse update: %+v", update)
	default:
	}

	state.CommitInventory([]*AgentState{{
		PaneID: "pane-1", Agent: "claude", Status: "done",
	}}, state.RevisionCounter())
	if result := d.handleAcknowledge("done", "pane-1"); !result.OK {
		t.Fatalf("done acknowledgement = %+v", result)
	}
	select {
	case update := <-updates:
		if update.Agent.Status != "idle" {
			t.Fatalf("acknowledgement update = %+v", update)
		}
	case <-time.After(time.Second):
		t.Fatal("displayed done-to-idle transition was not broadcast")
	}
}

func TestQuestionNavigationHasRequestIdentityAndReplaysTerminalInteraction(t *testing.T) {
	firstView := "Question 1/2 (1 unanswered)\nChoose the first value\n\n❯ 1. Alpha\n  2. Beta\n  3. None of the above\n\ntab to add notes | enter to submit answer | ←/→ to navigate questions"
	secondView := "Question 2/2 (1 unanswered)\nChoose the second value\n\n❯ 1. Gamma\n  2. Delta\n  3. None of the above\n\ntab to add notes | enter to submit all | ←/→ to navigate questions"
	client := newFakeBackend()
	client.contents["pane-1"] = secondView
	client.setHook("SendKeys", func(_ context.Context, call fakeCall) error {
		keys := call.Args[1].([]string)
		switch {
		case slices.Contains(keys, "Left"):
			client.update(func(b *fakeBackend) { b.contents["pane-1"] = firstView })
		case slices.Contains(keys, "Enter"):
			client.update(func(b *fakeBackend) { b.contents["pane-1"] = secondView })
		}
		return nil
	})

	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Agent: "codex", Status: "blocked"}}, state.RevisionCounter())
	d := newTestDispatcher(t, client, state)
	updates := make(chan map[string]any, 8)
	d.SetBroadcast(func(message any) {
		if update, ok := message.(map[string]any); ok {
			updates <- update
		}
	})
	second := question.Parse(secondView, "codex")
	first := question.Parse(firstView, "codex")
	if first == nil || second == nil {
		t.Fatalf("question fixtures did not parse: first=%+v second=%+v", first, second)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	navigate := func(requestID string, interaction *question.Interaction) *CommandResult {
		return d.Handle(ctx, protocol.Inbound{
			Type:          "navigate_question",
			RequestID:     requestID,
			PaneID:        "pane-1",
			InteractionID: interaction.ID,
			Direction:     "previous",
		})
	}
	awaitUpdate := func(requestID, phase string) map[string]any {
		t.Helper()
		for {
			select {
			case update := <-updates:
				if update["request_id"] == requestID && update["phase"] == phase {
					return update
				}
			case <-ctx.Done():
				t.Fatalf("no %s update for %s", phase, requestID)
			}
		}
	}

	if result := navigate("previous-1", second); !result.OK || result.Phase != "accepted" {
		t.Fatalf("first navigation = %+v", result)
	}
	firstUpdate := awaitUpdate("previous-1", "navigated")
	firstData, _ := firstUpdate["data"].(map[string]any)
	firstInteraction, _ := firstData["interaction"].(*question.Interaction)
	if firstInteraction == nil || firstInteraction.QuestionIndex != 1 {
		t.Fatalf("first navigation data = %#v", firstUpdate["data"])
	}
	replay := navigate("previous-1", second)
	replayedData, _ := replay.Data.(map[string]any)
	replayedInteraction, _ := replayedData["interaction"].(*question.Interaction)
	if !replay.OK || replay.Phase != "navigated" || replayedInteraction == nil ||
		replayedInteraction.QuestionIndex != 1 {
		t.Fatalf("navigation replay = %+v", replay)
	}

	answer := d.Handle(ctx, protocol.Inbound{
		Type:            "answer_question",
		RequestID:       "answer-1",
		PaneID:          "pane-1",
		InteractionID:   first.ID,
		SelectedIndices: []int{0},
	})
	if !answer.OK || answer.Phase != "accepted" {
		t.Fatalf("answer = %+v", answer)
	}
	awaitUpdate("answer-1", "advanced")

	if result := navigate("previous-2", second); !result.OK || result.Phase != "accepted" {
		t.Fatalf("second navigation = %+v", result)
	}
	awaitUpdate("previous-2", "navigated")

	changedAnswer := d.Handle(ctx, protocol.Inbound{
		Type:            "answer_question",
		RequestID:       "answer-2",
		PaneID:          "pane-1",
		InteractionID:   first.ID,
		SelectedIndices: []int{1},
	})
	if !changedAnswer.OK || changedAnswer.Phase != "accepted" {
		t.Fatalf("changed answer after returning to question = %+v", changedAnswer)
	}
	awaitUpdate("answer-2", "advanced")

	if lefts := keyCalls(client, "Left"); lefts != 2 {
		t.Fatalf("Left dispatches = %d, want 2: %+v", lefts, client.recorded())
	}
	if enters := keyCalls(client, "Enter"); enters != 2 {
		t.Fatalf("answer dispatches = %d, want 2: %+v", enters, client.recorded())
	}
}

func TestUnchangedQuestionNavigationReturnsReplayableError(t *testing.T) {
	questionView := "Question 2/2 (1 unanswered)\nChoose the second value\n\n❯ 1. Gamma\n  2. Delta\n  3. None of the above\n\ntab to add notes | enter to submit all | ←/→ to navigate questions"
	interaction := question.Parse(questionView, "codex")
	if interaction == nil {
		t.Fatal("question fixture did not parse")
	}
	client := newFakeBackend()
	client.contents["pane-1"] = questionView
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Agent: "codex", Status: "blocked"}}, state.RevisionCounter())
	d := newTestDispatcher(t, client, state)
	updates := make(chan map[string]any, 1)
	d.SetBroadcast(func(message any) {
		if update, ok := message.(map[string]any); ok {
			updates <- update
		}
	})
	message := protocol.Inbound{
		Type:          "navigate_question",
		RequestID:     "unchanged",
		PaneID:        "pane-1",
		InteractionID: interaction.ID,
		Direction:     "previous",
	}
	if accepted := d.Handle(context.Background(), message); !accepted.OK || accepted.Phase != "accepted" {
		t.Fatalf("accepted = %+v", accepted)
	}
	select {
	case update := <-updates:
		if update["ok"] != false || update["phase"] != "unconfirmed" ||
			!strings.Contains(fmt.Sprint(update["error"]), "still shows the same question") {
			t.Fatalf("unconfirmed update = %+v", update)
		}
	case <-time.After(approvalPollTimeout + 2*time.Second):
		t.Fatal("unchanged navigation did not fail")
	}
	replay := d.Handle(context.Background(), message)
	if replay.OK || replay.Phase != "unconfirmed" ||
		!strings.Contains(replay.Error, "still shows the same question") {
		t.Fatalf("unconfirmed replay = %+v", replay)
	}
}

func TestSchedulerEvictsOldCompletedLedgerEntries(t *testing.T) {
	scheduler := newTestScheduler(t, 1)

	executions := make(map[string]int)
	run := func(key string) *CommandResult {
		now := time.Now()
		result, err := scheduler.Execute(context.Background(), ScheduleOptions{
			Command: Command{
				ID: scheduler.NextCommandID(), RequestID: key, ReceivedAt: now,
				Deadline: now.Add(time.Second), Action: "agent_start",
			},
			RelayLevel:  true,
			LedgerKey:   key,
			PayloadHash: "same",
		}, EffectFunc(func(context.Context, WorkerToken) EffectResult {
			executions[key]++
			return EffectResult{Result: completed(key, "agent_start", "", nil)}
		}))
		if err != nil {
			t.Fatal(err)
		}
		return result
	}

	for index := 0; index < maxLedgerEntries+8; index++ {
		run(fmt.Sprintf("start-%03d", index))
	}
	run("start-000")
	if executions["start-000"] != 2 {
		t.Fatalf("old ledger entry executions = %d, want eviction and rerun", executions["start-000"])
	}
	latest := fmt.Sprintf("start-%03d", maxLedgerEntries+7)
	result := run(latest)
	if executions[latest] != 1 || result == nil || !result.replayed {
		t.Fatalf("latest ledger entry was not replayed: executions=%d result=%+v", executions[latest], result)
	}
}
