package coordinator

// Regression tests for dispatch-path safety invariants (ADR-001 idempotency,
// §9.3/§11.2 dispatched_unknown). They drive the real Dispatcher against a
// Herdr binary implemented as a tiny inline script.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
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

// T7 — §9.6 / §16.7: two clients approving the same (pane, event_id, index)
// must dispatch the approval keystroke exactly once.
func TestDuplicateApprovalDispatchesOnce(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "invocations.log")
	// Records every invocation's argv, then returns success.
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"printf '%s\\n' \"$*\" >> \""+record+"\"\n"+
		"printf '{\"ok\":true}\\n'\n")

	d := NewDispatcher(herdr.NewClient(bin, filepath.Join(dir, "sock")), NewState(testLogger()), nil, testLogger())
	d.state.CommitInventory([]*AgentState{{PaneID: "pane-1", Status: "blocked"}}, d.state.RevisionCounter())
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

// T10 — §9.3 / §11.2: a command whose herdr subprocess was started but then
// timed out must be reported as dispatched_unknown, never a plain failure —
// otherwise the phone may safely-retry an approval/prompt that already ran.
func TestPostDispatchTimeoutIsDispatchedUnknown(t *testing.T) {
	dir := t.TempDir()
	// Ignores its arguments and hangs, so the subprocess is definitely started
	// and then killed by the deadline (post-dispatch timeout).
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\nsleep 30\n")

	d := NewDispatcher(herdr.NewClient(bin, filepath.Join(dir, "sock")), NewState(testLogger()), nil, testLogger())

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
