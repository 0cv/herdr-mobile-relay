package coordinator

// Regression tests for dispatch-path idempotency and dispatched-unknown
// outcomes. They drive the real Dispatcher against the in-process Herdr fake.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/question"
)

func TestDispatcherRequiresDirectActionType(t *testing.T) {
	for _, admitted := range []bool{false, true} {
		for _, typ := range []string{"", "command"} {
			d := newTestDispatcher(t, nil, NewState(testLogger()))
			message := map[string]any{"type": typ, "action": "send_secret", "request_id": "unsupported", "pane_id": "pane", "text": "fixture"}
			var result *CommandResult
			signals := 0
			if admitted {
				result = d.HandleAdmitted(context.Background(), message, func() { signals++ })
			} else {
				result = d.Handle(context.Background(), message)
			}
			if result == nil || result.OK || result.Phase != "failed" || result.RequestID != "unsupported" || result.Action != typ || result.Error != "Unknown command" {
				t.Fatalf("result = %#v", result)
			}
			if admitted && signals != 1 {
				t.Fatalf("admission signals = %d", signals)
			}
		}
	}
}

func blockedEventID(t *testing.T, d *Dispatcher, paneID string) string {
	t.Helper()
	agent, ok := d.state.Agent(paneID)
	if !ok || agent.BlockedEventID == "" {
		t.Fatalf("blocked pane %q has no event ID", paneID)
	}
	return agent.BlockedEventID
}
func approvalIdentity(t *testing.T, d *Dispatcher, paneID string, index int) (string, string) {
	t.Helper()
	agent, ok := d.state.Agent(paneID)
	if !ok || agent.ApprovalFingerprint == "" || index < 0 || index >= len(agent.Options) {
		t.Fatalf("blocked pane %q has no exact approval identity", paneID)
	}
	return agent.ApprovalFingerprint, agent.Options[index]
}

const approvalPane = `Would you like to run this command?
$ make check
❯ 1. Approve
  2. Reject
Enter to select · Esc to cancel`

func commitApproval(state *State, paneID string) {
	classification := question.Classify(approvalPane, "codex")
	state.CommitInventory([]*AgentState{{
		PaneID: paneID, Agent: "codex", Status: "blocked",
		AttentionKind:       question.AttentionApproval,
		Prompt:              classification.Prompt,
		Command:             classification.Command,
		Options:             append([]string(nil), classification.Options...),
		ApprovalFingerprint: question.ApprovalFingerprint(classification),
	}}, state.RevisionCounter())
}

// T7 — §9.6 / §16.7: two clients approving the same (pane, event_id, index)
// must dispatch the approval keystroke exactly once.
func TestDuplicateApprovalDispatchesOnce(t *testing.T) {
	client := newFakeBackend()
	client.contents["pane-1"] = approvalPane
	d := newTestDispatcher(t, client, NewState(testLogger()))
	commitApproval(d.state, "pane-1")
	eventID := blockedEventID(t, d, "pane-1")
	fingerprint, choice := approvalIdentity(t, d, "pane-1", 0)

	approve := func(reqID string) {
		d.Handle(context.Background(), map[string]any{
			"type":                 "respond",
			"request_id":           reqID,
			"pane_id":              "pane-1",
			"event_id":             eventID,
			"approval_fingerprint": fingerprint,
			"choice":               choice,
			"index":                float64(0),
			"total":                float64(2),
		})
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); approve("client-a") }()
	go func() { defer wg.Done(); approve("client-b") }()
	wg.Wait()

	if sends := client.callsTo("SendKeys"); len(sends) != 1 {
		t.Fatalf("duplicate approval dispatched %d SendKeys calls, want 1\ncalls: %+v", len(sends), client.recorded())
	}
}

func TestApprovalRevalidationRejectsCompletedNumberedProse(t *testing.T) {
	chatPane := `• Resolution plan
  1. Add parser fixtures.
  2. Verify the backend.
─ Worked for 4s ─
›`
	client := newFakeBackend()
	client.contents["pane-1"] = chatPane
	state := NewState(testLogger())

	commitApproval(state, "pane-1")
	d := newTestDispatcher(t, client, state)
	eventID := blockedEventID(t, d, "pane-1")
	fingerprint, choice := approvalIdentity(t, d, "pane-1", 0)

	result := d.Handle(context.Background(), map[string]any{
		"type": "respond", "request_id": "stale", "pane_id": "pane-1",
		"event_id": eventID, "approval_fingerprint": fingerprint, "choice": choice,
		"index": float64(0), "total": float64(2),
	})
	if result.OK || result.Error != "Approval choices are no longer available" {
		t.Fatalf("approval result = %+v", result)
	}
	if sends := client.callsTo("SendKeys"); len(sends) != 0 {
		t.Fatalf("approval keys were sent after chat revalidation: %+v", sends)
	}
}
func TestApprovalRevalidationRejectsSameLengthChangedChoices(t *testing.T) {
	changedPane := `Would you like to run this command?
$ make deploy
❯ 1. Run
  2. Cancel
Enter to select · Esc to cancel`
	client := newFakeBackend()
	client.contents["pane-1"] = changedPane
	state := NewState(testLogger())
	commitApproval(state, "pane-1")
	d := newTestDispatcher(t, client, state)
	fingerprint, choice := approvalIdentity(t, d, "pane-1", 0)
	result := d.Handle(context.Background(), map[string]any{
		"type": "respond", "request_id": "changed", "pane_id": "pane-1",
		"event_id": blockedEventID(t, d, "pane-1"), "approval_fingerprint": fingerprint,
		"choice": choice, "index": float64(0), "total": float64(2),
	})
	if result.OK || result.Error != "Approval choices are no longer available" {
		t.Fatalf("changed approval result = %+v", result)
	}
	if sends := client.callsTo("SendKeys"); len(sends) != 0 {
		t.Fatalf("approval keys were sent after same-length choices changed: %+v", sends)
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

	client := newFakeBackend()
	client.contents["pane-1"] = string(fixture)
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{
		PaneID: "pane-1", Agent: "qodercli", Status: "blocked",
		AttentionKind:       question.AttentionApproval,
		Options:             append([]string(nil), classification.Options...),
		ApprovalFingerprint: question.ApprovalFingerprint(classification),
	}}, state.RevisionCounter())
	d := newTestDispatcher(t, client, state)
	fingerprint, choice := approvalIdentity(t, d, "pane-1", 0)
	result := d.Handle(context.Background(), map[string]any{
		"type":                 "respond",
		"request_id":           "focus-aware",
		"pane_id":              "pane-1",
		"event_id":             blockedEventID(t, d, "pane-1"),
		"approval_fingerprint": fingerprint,
		"choice":               choice,
		"index":                float64(0),
		"total":                float64(len(classification.Options)),
	})
	if !result.OK || result.Phase != "accepted" {
		t.Fatalf("approval result = %+v", result)
	}
	want := []fakeCall{{Method: "SendKeys", Args: []any{"pane-1", []string{"Up", "Up", "Enter"}}}}
	if got := client.callsTo("SendKeys"); !reflect.DeepEqual(got, want) {
		t.Fatalf("approval keys = %+v, want %+v", got, want)
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
	client := newFakeBackend()
	client.setHook("Prompt", func(ctx context.Context, _ fakeCall) error {
		<-ctx.Done()
		return &herdr.OutcomeError{Started: true, Err: ctx.Err()}
	})
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Status: "working"}}, state.RevisionCounter())
	d := newTestDispatcher(t, client, state)

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	res := d.Handle(ctx, map[string]any{
		"type":       "submit_prompt",
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

func TestProtocolMismatchIsActionableAndNotRetryable(t *testing.T) {
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
		"submit_prompt",
		"pane-1",
		&herdr.OutcomeError{Started: true, Err: &herdr.CLIError{
			Code: "protocol_mismatch", Message: "server protocol 7 is not supported",
		}},
	)
	if result.Phase != "not_started" || result.Error != herdr.RefusalMessage("protocol_mismatch") {
		t.Fatalf("result = %+v, want controlled pre-dispatch refusal", result)
	}
	data, _ := result.Data.(map[string]any)
	if data["code"] != "protocol_mismatch" || data["dispatched_unknown"] == true {
		t.Fatalf("data = %+v, want exact refusal code without uncertainty", result.Data)
	}
	if herdr.IsTransientRefused(&herdr.CLIError{Code: "protocol_mismatch"}) {
		t.Fatal("protocol mismatch entered the transient retry class")
	}
}

func TestPartiallyAppliedOutranksProtocolMismatch(t *testing.T) {
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
		"answer_question",
		"pane-1",
		partiallyApplied(
			"earlier question input was already applied",
			&herdr.OutcomeError{Started: true, Err: &herdr.CLIError{Code: "protocol_mismatch"}},
		),
	)
	if result.Phase != "dispatched_unknown" || result.Data.(map[string]any)["dispatched_unknown"] != true {
		t.Fatalf("result = %+v, want uncertain partial outcome", result)
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
