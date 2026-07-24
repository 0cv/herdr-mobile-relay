package coordinator

// Idempotency regressions beyond approvals: pre-pane Agent Start (§9.6/§16.9)
// and structured-question answers (§9.6).

import (
	"context"
	"encoding/json"
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

func TestApprovalRetryAfterStatusChangeReturnsStoredPhase(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "approvals.log")
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"printf '%s\\n' \"$*\" >> \""+record+"\"\n"+
		"if [ \"$1 $2\" = \"pane read\" ]; then\n"+
		"  printf 'Run command?\\n1. Approve\\n2. Reject\\n'\n"+
		"else\n"+
		"  printf '{\"ok\":true}\\n'\n"+
		"fi\n")
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Status: "blocked"}}, state.RevisionCounter())
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

	answer := func() {
		d.Handle(context.Background(), map[string]any{
			"action":           "answer_question",
			"request_id":       "q",
			"pane_id":          "pane-1",
			"interaction_id":   interaction.ID,
			"selected_indices": []any{float64(0)},
		})
	}

	answer() // first submission
	answer() // duplicate: retry or a second client on the same interaction

	data, _ := os.ReadFile(record)
	// Each answer confirms with a single Enter; a deduped answer sends it once.
	enters := strings.Count(string(data), "Enter")
	if enters != 1 {
		t.Fatalf("duplicate question answer confirmed %d times, want 1\nrecord:\n%s", enters, data)
	}
}
