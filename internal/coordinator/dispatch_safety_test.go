package coordinator

// Regression tests for dispatch-path idempotency and dispatched-unknown
// outcomes. They drive the real Dispatcher against a
// Herdr binary implemented as a tiny inline script.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/question"
)

func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return p
}

func blockedEventID(t *testing.T, d *Dispatcher, paneID string) string {
	t.Helper()
	agent, ok := d.state.Agent(paneID)
	if !ok || agent.BlockedEventID == "" {
		t.Fatalf("blocked pane %q has no event ID", paneID)
	}
	return agent.BlockedEventID
}

const approvalPane = `Would you like to run this command?
$ make check
❯ 1. Approve
  2. Reject
Enter to select · Esc to cancel`

func commitApproval(state *State, paneID string) {
	state.CommitInventory([]*AgentState{{
		PaneID: paneID, Agent: "codex", Status: "blocked",
		AttentionKind: "approval", Options: []string{"Approve", "Reject"},
	}}, state.RevisionCounter())
}

// T7 — §9.6 / §16.7: two clients approving the same (pane, event_id, index)
// must dispatch the approval keystroke exactly once.
func TestDuplicateApprovalDispatchesOnce(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "invocations.log")
	// Records every invocation's argv, then returns success.
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"printf '%s\\n' \"$*\" >> \""+record+"\"\n"+
		"if [ \"$1 $2\" = \"pane read\" ]; then\n"+
		"  printf '"+approvalPane+"\\n'\n"+
		"else\n"+
		"  printf '{\"ok\":true}\\n'\n"+
		"fi\n")

	d := NewDispatcher(herdr.NewClient(bin, filepath.Join(dir, "sock")), NewState(testLogger()), nil, testLogger())
	commitApproval(d.state, "pane-1")
	eventID := blockedEventID(t, d, "pane-1")

	approve := func(reqID string) {
		d.Handle(context.Background(), map[string]any{
			"action":     "respond",
			"request_id": reqID,
			"pane_id":    "pane-1",
			"event_id":   eventID,
			"index":      float64(0),
			"total":      float64(2),
		})
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); approve("client-a") }()
	go func() { defer wg.Done(); approve("client-b") }()
	wg.Wait()

	data, _ := os.ReadFile(record)
	sends := strings.Count(string(data), "send-keys")
	if sends != 1 {
		t.Fatalf("duplicate approval dispatched %d send-keys invocations, want 1\nrecord:\n%s", sends, data)
	}
}

func TestApprovalRevalidationRejectsCompletedNumberedProse(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "invocations.log")
	chatPane := `• Resolution plan
  1. Add parser fixtures.
  2. Verify the backend.
─ Worked for 4s ─
›`
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"printf '%s\\n' \"$*\" >> \""+record+"\"\n"+
		"if [ \"$1 $2\" = \"pane read\" ]; then\n"+
		"  printf '"+chatPane+"\\n'\n"+
		"else\n"+
		"  printf '{\"ok\":true}\\n'\n"+
		"fi\n")
	state := NewState(testLogger())
	commitApproval(state, "pane-1")
	d := NewDispatcher(herdr.NewClient(bin, filepath.Join(dir, "sock")), state, nil, testLogger())
	eventID := blockedEventID(t, d, "pane-1")

	result := d.Handle(context.Background(), map[string]any{
		"action": "respond", "request_id": "stale", "pane_id": "pane-1",
		"event_id": eventID, "index": float64(0), "total": float64(2),
	})
	if result.OK || result.Error != "Approval choices are no longer available" {
		t.Fatalf("approval result = %+v", result)
	}
	data, _ := os.ReadFile(record)
	if strings.Contains(string(data), "send-keys") {
		t.Fatalf("approval keys were sent after chat revalidation:\n%s", data)
	}
}

func TestApprovalDispatchNavigatesFromReparsedLiveFocus(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join(
		"..",
		"question",
		"testdata",
		"attention",
		"qodercli-permission-required2.ansi",
	))
	if err != nil {
		t.Fatal(err)
	}
	classification := question.Classify(string(fixture), "qodercli")
	if classification.Kind != question.AttentionApproval ||
		classification.ApprovalFocus != 2 {
		t.Fatalf("classification = %+v", classification)
	}

	dir := t.TempDir()
	panePath := filepath.Join(dir, "pane.ansi")
	if err := os.WriteFile(panePath, fixture, 0o600); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(dir, "invocations.log")
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"printf '%s\\n' \"$*\" >> \""+record+"\"\n"+
		"if [ \"$1 $2\" = \"pane read\" ]; then\n"+
		"  cat \""+panePath+"\"\n"+
		"else\n"+
		"  printf '{\"ok\":true}\\n'\n"+
		"fi\n")

	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{
		PaneID: "pane-1", Agent: "qodercli", Status: "blocked",
		AttentionKind: question.AttentionApproval,
		Options:       append([]string(nil), classification.Options...),
	}}, state.RevisionCounter())
	d := NewDispatcher(
		herdr.NewClient(bin, filepath.Join(dir, "sock")),
		state,
		nil,
		testLogger(),
	)
	result := d.Handle(context.Background(), map[string]any{
		"action":     "respond",
		"request_id": "focus-aware",
		"pane_id":    "pane-1",
		"event_id":   blockedEventID(t, d, "pane-1"),
		"index":      float64(0),
		"total":      float64(len(classification.Options)),
	})
	if !result.OK || result.Phase != "accepted" {
		t.Fatalf("approval result = %+v", result)
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(
		string(data),
		"pane send-keys pane-1 Up Up Enter",
	) {
		t.Fatalf("focus-aware approval keys were not dispatched:\n%s", data)
	}
	if strings.Contains(string(data), "Escape") {
		t.Fatalf("approval dispatch assumed Escape selected the last row:\n%s", data)
	}
}

func TestApprovalKeysNavigateToEveryRowWithEnter(t *testing.T) {
	tests := []struct {
		name    string
		target  int
		current int
		want    []string
	}{
		{"earlier row", 0, 2, []string{"Up", "Up", "Enter"}},
		{"current row", 2, 2, []string{"Enter"}},
		{"later row", 4, 2, []string{"Down", "Down", "Enter"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := approvalKeys(test.target, test.current); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("approval keys = %#v, want %#v", got, test.want)
			}
		})
	}
}

// T10 — §9.3 / §11.2: a command whose herdr subprocess was started but then
// timed out must be reported as dispatched_unknown, never a plain failure —
// otherwise the phone may safely-retry an approval/prompt that already ran.
func TestPostDispatchTimeoutIsDispatchedUnknown(t *testing.T) {
	dir := t.TempDir()
	// Ignores its arguments and hangs, so the subprocess is definitely started
	// and then killed by the deadline (post-dispatch timeout).
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\nsleep 30\n")

	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Status: "working"}}, state.RevisionCounter())
	d := NewDispatcher(herdr.NewClient(bin, filepath.Join(dir, "sock")), state, nil, testLogger())

	// A short parent deadline wins over the handler's internal command deadline,
	// so the herdr child is started and then cancelled quickly.
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	res := d.Handle(ctx, map[string]any{
		"action":     "submit_prompt",
		"request_id": "r1",
		"pane_id":    "pane-1",
		"text":       "hello",
	})

	if res.Phase != "dispatched_unknown" {
		t.Fatalf("post-dispatch timeout reported phase %q, want %q "+
			"(no not_started/dispatched_unknown classification at the dispatch boundary)",
			res.Phase, "dispatched_unknown")
	}
}

func TestUnknownCreatedTargetGetsActionableUnsafeRetryError(t *testing.T) {
	d := NewDispatcher(nil, NewState(testLogger()), nil, testLogger())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := d.Close(ctx); err != nil {
			t.Fatalf("close dispatcher: %v", err)
		}
	})

	result := d.failErr(
		"request-1",
		"agent_start",
		"",
		errors.Join(herdr.ErrDispatchedUnknown, herdr.ErrCreatedTargetUnknown),
	)
	if result.Phase != "dispatched_unknown" {
		t.Fatalf("phase = %q, want dispatched_unknown", result.Phase)
	}
	if result.Error != "Herdr may have created an empty target; review Herdr before retrying" {
		t.Fatalf("error = %q, want actionable created-target warning", result.Error)
	}
}

func TestServerNotRunningIsSafeRetry(t *testing.T) {
	d := NewDispatcher(nil, NewState(testLogger()), nil, testLogger())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := d.Close(ctx); err != nil {
			t.Fatalf("close dispatcher: %v", err)
		}
	})

	// The realistic shape: the subprocess started, so OutcomeError.Unwrap puts
	// ErrDispatchedUnknown in the chain alongside the CLIError.
	result := d.failErr(
		"request-1",
		"submit_prompt",
		"pane-1",
		&herdr.OutcomeError{Started: true, Err: &herdr.CLIError{Code: "server_not_running", Message: "no server"}},
	)
	if result.Phase != "not_started" || result.Error != "Command was not sent; retry is safe" {
		t.Fatalf("result = %+v, want safe retry classification", result)
	}
}

// A later step failing with server_not_running must never be advertised as safe
// to retry once an earlier step already reached the agent: retrying would
// duplicate the input that landed.
func TestPartiallyAppliedOutranksServerNotRunning(t *testing.T) {
	d := NewDispatcher(nil, NewState(testLogger()), nil, testLogger())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := d.Close(ctx); err != nil {
			t.Fatalf("close dispatcher: %v", err)
		}
	})

	stepErr := &herdr.OutcomeError{Started: true, Err: &herdr.CLIError{Code: "server_not_running", Message: "no server"}}
	result := d.failErr(
		"request-1",
		"answer_question",
		"pane-1",
		partiallyApplied("earlier question input was already applied", stepErr),
	)
	if result.Phase != "dispatched_unknown" {
		t.Fatalf("phase = %q, want dispatched_unknown for a partially applied mutation", result.Phase)
	}
	if result.Error != "Part of the command already reached the agent; review it before retrying" {
		t.Fatalf("error = %q, want partially-applied warning", result.Error)
	}
	data, _ := result.Data.(map[string]any)
	if data["dispatched_unknown"] != true {
		t.Fatalf("data = %+v, want dispatched_unknown flag for the client retry guard", result.Data)
	}
}

// Issue #8: Herdr refuses agent.start with agent_pane_busy when the target
// pane has not reached a prompt. The refusal proves nothing ran, so telling
// the user to review an agent that never existed both misleads them and blocks
// the retry the situation calls for.
func TestAgentPaneBusyIsSafeRetry(t *testing.T) {
	d := NewDispatcher(nil, NewState(testLogger()), nil, testLogger())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := d.Close(ctx); err != nil {
			t.Fatalf("close dispatcher: %v", err)
		}
	})

	result := d.failErr(
		"request-1",
		"agent_start",
		"pane-1",
		&herdr.OutcomeError{Started: true, Err: &herdr.CLIError{
			Code:    "agent_pane_busy",
			Message: "agent target pane wH:p1 is not an available shell",
		}},
	)
	if result.Phase != "not_started" || result.Error != "Command was not sent; retry is safe" {
		t.Fatalf("result = %+v, want safe retry classification", result)
	}
	if result.Data != nil {
		t.Fatalf("data = %+v, want no dispatched_unknown guard on a refused command", result.Data)
	}
}

// The refusal classification must not become an escape hatch: a refused later
// step still leaves the earlier applied input in place.
func TestPartiallyAppliedOutranksAgentPaneBusy(t *testing.T) {
	d := NewDispatcher(nil, NewState(testLogger()), nil, testLogger())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := d.Close(ctx); err != nil {
			t.Fatalf("close dispatcher: %v", err)
		}
	})

	stepErr := &herdr.OutcomeError{Started: true, Err: &herdr.CLIError{Code: "agent_pane_busy", Message: "not a shell"}}
	result := d.failErr(
		"request-1",
		"agent_clear",
		"pane-1",
		partiallyApplied("replacement agent was already started", stepErr),
	)
	if result.Phase != "dispatched_unknown" {
		t.Fatalf("phase = %q, want dispatched_unknown for a partially applied mutation", result.Phase)
	}
	if result.Error != "Part of the command already reached the agent; review it before retrying" {
		t.Fatalf("error = %q, want partially-applied warning", result.Error)
	}
}

func TestCapPaneContentLines(t *testing.T) {
	tests := []struct {
		name    string
		content string
		limit   int
		want    string
	}{
		{name: "within limit", content: "one\ntwo\n", limit: 2, want: "one\ntwo\n"},
		{name: "without trailing newline", content: "one\ntwo\nthree", limit: 2, want: "two\nthree"},
		{name: "with trailing newline", content: "one\ntwo\nthree\n", limit: 2, want: "two\nthree\n"},
		{name: "retains trailing blank line", content: "one\ntwo\n\n", limit: 1, want: "\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := string(capPaneContentLines([]byte(test.content), test.limit))
			if got != test.want {
				t.Fatalf("capPaneContentLines() = %q, want %q", got, test.want)
			}
		})
	}
}
