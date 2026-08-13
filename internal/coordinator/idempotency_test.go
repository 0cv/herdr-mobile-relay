package coordinator

// Idempotency regressions beyond approvals: pre-pane Agent Start (§9.6/§16.9)
// and structured-question answers (§9.6).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/question"
)

func recordingHerdr(t *testing.T, dir, record, stdout string) string {
	return writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"printf '%s\\n' \"$*\" >> \""+record+"\"\n"+
		"printf '"+stdout+"\\n'\n")
}

// §9.6 / §16.9: two Agent Start requests with the same identity (client +
// request_id + action) must create at most one pane. There is no relay-level
// pre-pane scheduler ledger, so both callers observe the same pane creation.
func TestDuplicateAgentStartCreatesOnePane(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "starts.log")
	bin := recordingHerdr(t, dir, record, `{"result":{"pane_id":"pane-new"}}`)

	d := NewDispatcher(herdr.NewClient(bin, filepath.Join(dir, "sock")), NewState(testLogger()), nil, testLogger())

	start := func() {
		d.Handle(context.Background(), map[string]any{
			"action":     "agent_start",
			"request_id": "same-req",
			"profile_id": "claude",
			"name":       "proj",
			"cwd":        "/tmp",
		})
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); start() }()
	go func() { defer wg.Done(); start() }()
	wg.Wait()

	data, _ := os.ReadFile(record)
	starts := strings.Count(string(data), "--kind")
	if starts != 1 {
		t.Fatalf("duplicate agent_start created %d panes, want 1\nrecord:\n%s", starts, data)
	}
}

func TestAgentStartRetryDoesNotResubmitInitialPrompt(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "starts.log")
	bin := recordingHerdr(t, dir, record, `{"result":{"pane_id":"pane-new"}}`)

	// Pre-check: verify the script produces valid JSON output directly.
	preDir := filepath.Join(dir, "pre")
	os.MkdirAll(preDir, 0o755)
	preRecord := filepath.Join(preDir, "pre.log")
	preBin := recordingHerdr(t, preDir, preRecord, `{"result":{"pane_id":"pane-new"}}`)
	preCmd := exec.Command(preBin, "agent", "start", "pre", "--kind", "claude", "--pane", "p", "--timeout", "30000")
	preOut, preErr := preCmd.CombinedOutput()
	if preErr != nil {
		t.Fatalf("script pre-check failed: %v\noutput: %q", preErr, preOut)
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(preOut, &envelope); err != nil {
		t.Fatalf("script pre-check produced invalid JSON: %v\nraw output: %q", err, preOut)
	}

	d := NewDispatcher(herdr.NewClient(bin, filepath.Join(dir, "sock")), NewState(testLogger()), nil, testLogger())
	message := map[string]any{
		"action": "agent_start", "request_id": "same-req",
		"profile_id": "claude", "name": "proj", "cwd": "/tmp", "prompt": "hello",
	}
	first := d.Handle(context.Background(), message)
	second := d.Handle(context.Background(), message)
	if !first.OK || !second.OK {
		data, _ := os.ReadFile(record)
		t.Fatalf("start results = %+v, %+v\nrecord:\n%s", first, second, data)
	}
	data, _ := os.ReadFile(record)
	if starts := strings.Count(string(data), "--kind"); starts != 1 {
		t.Fatalf("start invocations = %d, want 1\n%s", starts, data)
	}
	if prompts := strings.Count(string(data), "agent prompt"); prompts != 1 {
		t.Fatalf("initial prompt invocations = %d, want 1\n%s", prompts, data)
	}
}

func TestLedgerReplayReturnsConfirmationWatchPhase(t *testing.T) {
	scheduler := NewScheduler(1, testLogger())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := scheduler.Close(ctx); err != nil {
			t.Errorf("close scheduler: %v", err)
		}
	})

	const ledgerKey = "approval\x00pane-1\x00event-1"
	options := func() ScheduleOptions {
		now := time.Now()
		return ScheduleOptions{
			Command: Command{
				ID:         scheduler.NextCommandID(),
				RequestID:  "approve",
				ReceivedAt: now,
				Deadline:   now.Add(time.Second),
				Kind:       CommandApproval,
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
	dir := t.TempDir()
	record := filepath.Join(dir, "stops.log")
	bin := recordingHerdr(t, dir, record, `{"ok":true}`)
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Status: "working"}}, state.RevisionCounter())
	d := NewDispatcher(herdr.NewClient(bin, filepath.Join(dir, "sock")), state, nil, testLogger())
	message := map[string]any{
		"action": "agent_stop", "request_id": "stop-1", "pane_id": "pane-1",
	}
	first := d.Handle(context.Background(), message)
	second := d.Handle(context.Background(), message)
	if !first.OK || !second.OK {
		t.Fatalf("stop results = %+v, %+v", first, second)
	}
	data, _ := os.ReadFile(record)
	if closes := strings.Count(string(data), "pane close"); closes != 1 {
		t.Fatalf("pane close invocations = %d, want 1\n%s", closes, data)
	}
	if generation := state.Generation("pane-1"); generation != 1 {
		t.Fatalf("state generation = %d, want 1", generation)
	}
}

func TestTabRenameAcceptsNaturalLabel(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "renames.log")
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{
		PaneID: "pane-1", TabID: "tab-1", Status: "working",
	}}, state.RevisionCounter())
	d := NewDispatcher(
		herdr.NewClient(recordingHerdr(t, dir, record, `{"ok":true}`), filepath.Join(dir, "sock")),
		state,
		nil,
		testLogger(),
	)

	result := d.Handle(context.Background(), map[string]any{
		"action": "agent_rename", "request_id": "rename-1", "pane_id": "pane-1", "name": "123",
	})
	if !result.OK {
		t.Fatalf("numeric rename failed: %+v", result)
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "tab rename tab-1 123") {
		t.Fatalf("rename commands = %q, missing tab rename", data)
	}
	if strings.Contains(string(data), "agent rename") {
		t.Fatalf("tab label was sent through agent rename: %q", data)
	}
}

func TestApprovalRetryAfterStatusChangeReturnsStoredPhase(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "approvals.log")
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"printf '%s\\n' \"$*\" >> \""+record+"\"\n"+
		"if [ \"$1 $2\" = \"pane read\" ]; then\n"+
		"  printf '"+approvalPane+"\\n'\n"+
		"else\n"+
		"  printf '{\"ok\":true}\\n'\n"+
		"fi\n")
	state := NewState(testLogger())
	commitApproval(state, "pane-1")
	eventID := blockedEventID(t, &Dispatcher{state: state}, "pane-1")
	d := NewDispatcher(herdr.NewClient(bin, filepath.Join(dir, "sock")), state, nil, testLogger())
	updates := make(chan map[string]any, 1)
	d.SetBroadcast(func(message any) {
		if update, ok := message.(map[string]any); ok {
			updates <- update
		}
	})
	message := map[string]any{
		"action": "respond", "request_id": "approval-1", "pane_id": "pane-1",
		"event_id": eventID, "index": float64(0), "total": float64(2),
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
	data, _ := os.ReadFile(record)
	if sends := strings.Count(string(data), "send-keys"); sends != 1 {
		t.Fatalf("approval sends = %d, want 1\n%s", sends, data)
	}
}

// §9.6: repeating answer_question with the same interaction_id must send the
// answer keys only once.
func TestDuplicateQuestionAnswerSendsOnce(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "keys.log")
	questionView := "Which deployment target?\n❯ 1. Development\n2. Staging\n3. Type something.\n4. Chat about this\nEnter to select · ↑/↓ to navigate · Esc to cancel"
	interaction := question.Parse(questionView, "claude")
	if interaction == nil {
		t.Fatal("test question did not parse")
	}
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"printf '%s\\n' \"$*\" >> \""+record+"\"\n"+
		"if [ \"$1 $2\" = \"pane read\" ]; then\n"+
		"  printf '%s\\n' '"+questionView+"'\n"+
		"else\n"+
		"  printf '{\"ok\":true}\\n'\n"+
		"fi\n")

	d := NewDispatcher(herdr.NewClient(bin, filepath.Join(dir, "sock")), NewState(testLogger()), nil, testLogger())
	d.state.CommitInventory([]*AgentState{{PaneID: "pane-1", Agent: "claude", Status: "blocked"}}, d.state.RevisionCounter())

	answer := func() *CommandResult {
		return d.Handle(context.Background(), map[string]any{
			"action":           "answer_question",
			"request_id":       "q",
			"pane_id":          "pane-1",
			"interaction_id":   interaction.ID,
			"selected_indices": []any{float64(0)},
		})
	}

	answer() // first submission
	answer() // duplicate: retry or a second client on the same interaction

	conflict := d.Handle(context.Background(), map[string]any{
		"action":           "answer_question",
		"request_id":       "q",
		"pane_id":          "pane-1",
		"interaction_id":   interaction.ID,
		"selected_indices": []any{float64(1)},
	})
	if conflict.OK || !strings.Contains(conflict.Error, "different response") {
		t.Fatalf("same request with different answer = %+v, want conflict", conflict)
	}

	data, _ := os.ReadFile(record)
	// Each answer confirms with a single Enter; a deduped answer sends it once.
	enters := strings.Count(string(data), "Enter")
	if enters != 1 {
		t.Fatalf("duplicate question answer confirmed %d times, want 1\nrecord:\n%s", enters, data)
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
			dir := t.TempDir()
			countFile := filepath.Join(dir, "reads")
			if err := os.WriteFile(countFile, []byte("0\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
				"if [ \"$1 $2 $3\" = \"pane read pane-1\" ]; then\n"+
				"  count=$(cat \""+countFile+"\")\n"+
				"  count=$((count + 1))\n"+
				"  printf '%s\\n' \"$count\" > \""+countFile+"\"\n"+
				"  if [ \"$count\" -eq 1 ]; then\n"+
				"    printf '%s\\n' '"+test.initialView+"'\n"+
				"  elif [ \"$count\" -eq 2 ]; then\n"+
				"    printf '%s\\n' 'redrawing question'\n"+
				"  else\n"+
				"    printf '%s\\n' '"+test.nextView+"'\n"+
				"  fi\n"+
				"else\n"+
				"  printf '{\"ok\":true}\\n'\n"+
				"fi\n")
			state := NewState(testLogger())
			state.CommitInventory([]*AgentState{{
				PaneID: "pane-1", Agent: "codex", Status: "blocked",
			}}, state.RevisionCounter())
			d := NewDispatcher(herdr.NewClient(bin, filepath.Join(dir, "sock")), state, nil, testLogger())
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_ = d.Close(ctx)
			})
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
			message := map[string]any{
				"action":         test.action,
				"request_id":     "transition",
				"pane_id":        "pane-1",
				"interaction_id": interaction.ID,
			}
			if test.direction != "" {
				message["direction"] = test.direction
			} else {
				message["selected_indices"] = []any{float64(0)}
			}
			if accepted := d.Handle(context.Background(), message); !accepted.OK || accepted.Phase != "accepted" {
				t.Fatalf("accepted result = %+v", accepted)
			}
			timeout := time.After(3 * time.Second)
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
				case <-timeout:
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
	d := NewDispatcher(nil, state, nil, testLogger())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = d.Close(ctx)
	})
	updates := make(chan map[string]any, 2)
	d.SetBroadcast(func(message any) {
		if update, ok := message.(map[string]any); ok {
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
		if update["status"] != "idle" {
			t.Fatalf("acknowledgement update = %+v", update)
		}
	case <-time.After(time.Second):
		t.Fatal("displayed done-to-idle transition was not broadcast")
	}
}

func TestQuestionNavigationHasRequestIdentityAndReplaysTerminalInteraction(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "keys.log")
	stateFile := filepath.Join(dir, "pane.txt")
	firstFile := filepath.Join(dir, "first.txt")
	secondFile := filepath.Join(dir, "second.txt")
	firstView := "Question 1/2 (1 unanswered)\nChoose the first value\n\n❯ 1. Alpha\n  2. Beta\n  3. None of the above\n\ntab to add notes | enter to submit answer | ←/→ to navigate questions"
	secondView := "Question 2/2 (1 unanswered)\nChoose the second value\n\n❯ 1. Gamma\n  2. Delta\n  3. None of the above\n\ntab to add notes | enter to submit all | ←/→ to navigate questions"
	if err := os.WriteFile(firstFile, []byte(firstView), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secondFile, []byte(secondView), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stateFile, []byte(secondView), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"printf '%s\\n' \"$*\" >> \""+record+"\"\n"+
		"if [ \"$1 $2 $3\" = \"pane read pane-1\" ]; then\n"+
		"  cat \""+stateFile+"\"\n"+
		"elif printf '%s' \"$*\" | grep -q 'Left'; then\n"+
		"  cp \""+firstFile+"\" \""+stateFile+"\"\n"+
		"  printf '{\"ok\":true}\\n'\n"+
		"elif printf '%s' \"$*\" | grep -q 'Enter'; then\n"+
		"  cp \""+secondFile+"\" \""+stateFile+"\"\n"+
		"  printf '{\"ok\":true}\\n'\n"+
		"else\n"+
		"  printf '{\"ok\":true}\\n'\n"+
		"fi\n")

	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Agent: "codex", Status: "blocked"}}, state.RevisionCounter())
	d := NewDispatcher(herdr.NewClient(bin, filepath.Join(dir, "sock")), state, nil, testLogger())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := d.Close(ctx); err != nil {
			t.Errorf("close dispatcher: %v", err)
		}
	})
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

	navigate := func(requestID string, interaction *question.Interaction) *CommandResult {
		return d.Handle(context.Background(), map[string]any{
			"action":         "navigate_question",
			"request_id":     requestID,
			"pane_id":        "pane-1",
			"interaction_id": interaction.ID,
			"direction":      "previous",
		})
	}
	awaitUpdate := func(requestID, phase string) map[string]any {
		t.Helper()
		timeout := time.After(2 * time.Second)
		for {
			select {
			case update := <-updates:
				if update["request_id"] == requestID && update["phase"] == phase {
					return update
				}
			case <-timeout:
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

	answer := d.Handle(context.Background(), map[string]any{
		"action":           "answer_question",
		"request_id":       "answer-1",
		"pane_id":          "pane-1",
		"interaction_id":   first.ID,
		"selected_indices": []any{float64(0)},
	})
	if !answer.OK || answer.Phase != "accepted" {
		t.Fatalf("answer = %+v", answer)
	}
	awaitUpdate("answer-1", "advanced")

	if result := navigate("previous-2", second); !result.OK || result.Phase != "accepted" {
		t.Fatalf("second navigation = %+v", result)
	}
	awaitUpdate("previous-2", "navigated")

	changedAnswer := d.Handle(context.Background(), map[string]any{
		"action":           "answer_question",
		"request_id":       "answer-2",
		"pane_id":          "pane-1",
		"interaction_id":   first.ID,
		"selected_indices": []any{float64(1)},
	})
	if !changedAnswer.OK || changedAnswer.Phase != "accepted" {
		t.Fatalf("changed answer after returning to question = %+v", changedAnswer)
	}
	awaitUpdate("answer-2", "advanced")

	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if lefts := strings.Count(string(data), "Left"); lefts != 2 {
		t.Fatalf("Left dispatches = %d, want 2\n%s", lefts, data)
	}
	if enters := strings.Count(string(data), "Enter"); enters != 2 {
		t.Fatalf("answer dispatches = %d, want 2\n%s", enters, data)
	}
}

func TestUnchangedQuestionNavigationReturnsReplayableError(t *testing.T) {
	dir := t.TempDir()
	questionView := "Question 2/2 (1 unanswered)\nChoose the second value\n\n❯ 1. Gamma\n  2. Delta\n  3. None of the above\n\ntab to add notes | enter to submit all | ←/→ to navigate questions"
	interaction := question.Parse(questionView, "codex")
	if interaction == nil {
		t.Fatal("question fixture did not parse")
	}
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"if [ \"$1 $2 $3\" = \"pane read pane-1\" ]; then\n"+
		"  printf '%s\\n' '"+questionView+"'\n"+
		"else\n"+
		"  printf '{\"ok\":true}\\n'\n"+
		"fi\n")
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Agent: "codex", Status: "blocked"}}, state.RevisionCounter())
	d := NewDispatcher(herdr.NewClient(bin, filepath.Join(dir, "sock")), state, nil, testLogger())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = d.Close(ctx)
	})
	updates := make(chan map[string]any, 1)
	d.SetBroadcast(func(message any) {
		if update, ok := message.(map[string]any); ok {
			updates <- update
		}
	})
	message := map[string]any{
		"action":         "navigate_question",
		"request_id":     "unchanged",
		"pane_id":        "pane-1",
		"interaction_id": interaction.ID,
		"direction":      "previous",
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
	scheduler := NewScheduler(1, testLogger())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := scheduler.Close(ctx); err != nil {
			t.Errorf("close scheduler: %v", err)
		}
	})

	executions := make(map[string]int)
	run := func(key string) *CommandResult {
		now := time.Now()
		result, err := scheduler.Execute(context.Background(), ScheduleOptions{
			Command: Command{
				ID: scheduler.NextCommandID(), RequestID: key, ReceivedAt: now,
				Deadline: now.Add(time.Second), Kind: CommandStart,
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
