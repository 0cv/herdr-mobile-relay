package coordinator

// Regression tests for the §10.3 stale-status pin and approval-ledger handling
// across dispatched_unknown outcomes. Each asserts the observable invariant,
// independent of the fix mechanism.
// Helpers testLogger/ts/writeScript live in safety_test.go and dispatch_safety_test.go.

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/activity"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func TestTransitionActivityPreservesExtract(t *testing.T) {
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{PaneID: "p1", Status: "working"}}, state.RevisionCounter())
	state.CommitEvent("p1", "blocked", ts())
	journal, err := activity.OpenJournal(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := NewDispatcher(nil, state, journal, testLogger())
	t.Cleanup(func() {
		_ = dispatcher.Close(context.Background())
	})
	revision := state.Revision("p1")
	if !dispatcher.RecordTransitionActivity(TransitionActivity{
		Kind:           "blocked",
		Status:         "attention",
		Summary:        "git push",
		PaneID:         "p1",
		ExpectedStatus: "blocked",
		Revision:       revision,
		Details:        map[string]any{"event_id": "event-1"},
		Agent:          "Claude",
		Project:        "project",
		Host:           "host",
		Session:        "session",
		Extract:        "Allow command?\n$ git push",
	}) {
		t.Fatal("current transition activity was not committed")
	}
	recent := journal.Recent(1)
	if len(recent) != 1 || recent[0].Summary != "git push" ||
		recent[0].Extract != "Allow command?\n$ git push" {
		t.Fatalf("transition activity = %+v", recent)
	}
}

// §10.3: a UDP event must not pin a pane's status indefinitely. Once fresh
// inventory polls (which necessarily started after the event) consistently
// report a different status, the poll is authoritative and must win. The
// current sticky eventTouched flag preserves the event's status until an
// inventory happens to report that exact status, so it stays stuck.
func TestEventDoesNotPinStaleStatus(t *testing.T) {
	s := NewState(testLogger())

	s.CommitInventory([]*AgentState{{PaneID: "p1", Status: "working"}}, s.RevisionCounter())

	// A UDP event reports "blocked" (e.g. a status hook).
	s.CommitEvent("p1", "blocked", ts())

	// The agent then diverges — resumed working after being answered directly
	// in the terminal — and no follow-up event fires. Several fresh polls, all
	// of which began after the event, agree the agent is working.
	for i := 0; i < 3; i++ {
		s.CommitInventory([]*AgentState{{PaneID: "p1", Status: "working"}}, s.RevisionCounter())
	}

	if got := s.DisplayedStatus("p1"); got != "working" {
		t.Fatalf("event pinned a stale status: DisplayedStatus = %q, want %q "+
			"(§10.3 must be poll-lifetime-bounded — a poll that started after the "+
			"event is authoritative; the sticky eventTouched flag pins it forever)",
			got, "working")
	}
}

// §9.6 / §16.7: after a dispatched_unknown outcome the approval keys MAY have
// been sent, so the ledger entry must be retained to block a retry. The current
// handler deletes the claim on any error, so a retry re-dispatches — the double
// execution the ledger exists to prevent.
func TestRetryAfterDispatchedUnknownDoesNotRedispatch(t *testing.T) {
	client := newFakeBackend()
	client.contents["pane-1"] = approvalPane
	client.setHook("SendKeys", func(ctx context.Context, _ fakeCall) error {
		<-ctx.Done()
		return &herdr.OutcomeError{Started: true, Err: ctx.Err()}
	})
	d := newTestDispatcher(t, client, NewState(testLogger()))
	commitApproval(d.state, "pane-1")
	eventID := blockedEventID(t, d, "pane-1")
	fingerprint, choice := approvalIdentity(t, d, "pane-1", 0)

	approve := func() *CommandResult {
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		return d.Handle(ctx, map[string]any{
			"type":                 "respond",
			"request_id":           "r1",
			"pane_id":              "pane-1",
			"event_id":             eventID,
			"approval_fingerprint": fingerprint,
			"choice":               choice,
			"index":                float64(0),
			"total":                float64(2),
		})
	}

	first := approve()
	if first.Phase != "dispatched_unknown" {
		t.Fatalf("precondition failed: first approval phase = %q, want dispatched_unknown", first.Phase)
	}

	// The client retries the same (pane, event_id, index) after the uncertain outcome.
	_ = approve()

	if sends := client.callsTo("SendKeys"); len(sends) != 1 {
		t.Fatalf("approval re-dispatched after dispatched_unknown: %d SendKeys calls, want 1 "+
			"(the ledger must retain the claim on an uncertain dispatch to block retry)\ncalls: %+v",
			len(sends), client.recorded())
	}
}
