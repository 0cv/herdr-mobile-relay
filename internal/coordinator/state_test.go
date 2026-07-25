package coordinator

import (
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestInventoryFailureClearsReadinessAndPreservesStaleSnapshot(t *testing.T) {
	state := NewState(slog.New(slog.NewTextHandler(io.Discard, nil)))
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Status: "idle"}}, 0)
	if !state.InventoryReady() {
		t.Fatal("inventory should be ready after a successful commit")
	}
	state.MarkInventoryFailure(errors.New("offline"))
	if state.InventoryReady() {
		t.Fatal("inventory remained ready after a failed poll")
	}
	status := state.InventoryStatus()
	if status["state"] != "error" || status["stale"] != true {
		t.Fatalf("inventory status = %+v", status)
	}
	if state.AgentCount() != 1 {
		t.Fatal("failed poll discarded the last known snapshot")
	}
}

func TestTopologyChurnMarksInventoryDegraded(t *testing.T) {
	state := testState()
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Status: "working"}}, 0)
	state.MarkTopologyDegraded()
	status := state.InventoryStatus()
	if status["state"] != "error" || status["error_code"] != "topology_churn" || status["stale"] != true {
		t.Fatalf("degraded inventory status = %+v", status)
	}
}

func testState() *State {
	return NewState(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestCommitInventoryAndSnapshot(t *testing.T) {
	s := testState()

	agents := []*AgentState{
		{PaneID: "p1", Status: "working", Agent: "claude"},
		{PaneID: "p2", Status: "idle", Agent: "codex"},
	}
	s.CommitInventory(agents, s.RevisionCounter())

	snap := s.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot len = %d, want 2", len(snap))
	}
	if s.AgentCount() != 2 {
		t.Errorf("agent count = %d, want 2", s.AgentCount())
	}
}

func TestOnlyInitialSnapshotUsesZeroUpdatedAt(t *testing.T) {
	s := testState()
	s.CommitInventory([]*AgentState{{PaneID: "p1", Status: "working"}}, s.RevisionCounter())
	first, _ := s.Agent("p1")
	if first.UpdatedAt != 0 {
		t.Fatalf("initial updated_at = %d, want 0", first.UpdatedAt)
	}
	s.CommitInventory(nil, s.RevisionCounter())
	s.CommitInventory([]*AgentState{{PaneID: "p1", Status: "working"}}, s.RevisionCounter())
	reappeared, _ := s.Agent("p1")
	if reappeared.UpdatedAt <= 0 {
		t.Fatalf("reappeared updated_at = %d, want epoch milliseconds", reappeared.UpdatedAt)
	}
}

func TestActivitySequenceAdvancesUpdatedAt(t *testing.T) {
	s := testState()
	s.CommitInventory([]*AgentState{{
		PaneID: "p1", Status: "idle", ActivitySeq: 10,
	}}, s.RevisionCounter())
	first, _ := s.Agent("p1")
	if first.UpdatedAt != 0 {
		t.Fatalf("initial updated_at = %d, want 0", first.UpdatedAt)
	}

	s.CommitInventory([]*AgentState{{
		PaneID: "p1", Status: "idle", ActivitySeq: 11,
	}}, s.RevisionCounter())
	updated, _ := s.Agent("p1")
	if updated.UpdatedAt <= 0 {
		t.Fatalf("activity update timestamp = %d, want epoch milliseconds", updated.UpdatedAt)
	}
	if updated.ActivitySeq != 11 {
		t.Fatalf("activity sequence = %d, want 11", updated.ActivitySeq)
	}
}

func TestDisplayedStatusUnseenDone(t *testing.T) {
	s := testState()

	s.CommitInventory([]*AgentState{
		{PaneID: "p1", Status: "working"},
	}, s.RevisionCounter())

	// Transition to done
	s.CommitEvent("p1", "done", 1000)
	if got := s.DisplayedStatus("p1"); got != "done" {
		t.Errorf("displayed = %q, want done", got)
	}

	// Transition to idle (should show "done" because unseen)
	s.CommitEvent("p1", "idle", 2000)
	if got := s.DisplayedStatus("p1"); got != "done" {
		t.Errorf("displayed after idle = %q, want done (unseen)", got)
	}

	// Acknowledge
	s.AcknowledgePane("p1")
	if got := s.DisplayedStatus("p1"); got != "idle" {
		t.Errorf("displayed after ack = %q, want idle", got)
	}
}

func TestAttentionClearsUnseen(t *testing.T) {
	s := testState()

	s.CommitInventory([]*AgentState{
		{PaneID: "p1", Status: "idle"},
	}, s.RevisionCounter())

	s.CommitEvent("p1", "done", 1000)
	if got := s.DisplayedStatus("p1"); got != "done" {
		t.Fatalf("expected done, got %q", got)
	}

	// New attention status clears unseen
	s.CommitEvent("p1", "working", 3000)
	if got := s.DisplayedStatus("p1"); got != "working" {
		t.Errorf("displayed = %q, want working", got)
	}

	// A new attention cycle followed by idle is a new unseen completion.
	s.CommitEvent("p1", "idle", 4000)
	if got := s.DisplayedStatus("p1"); got != "done" {
		t.Errorf("displayed = %q, want done", got)
	}
}

func TestTopologyGenerationBumps(t *testing.T) {
	s := testState()

	gen1 := s.TopologyGeneration()
	s.CommitInventory([]*AgentState{{PaneID: "p1", Status: "idle"}}, s.RevisionCounter())
	gen2 := s.TopologyGeneration()

	if gen2 <= gen1 {
		t.Errorf("topology gen did not bump: %d -> %d", gen1, gen2)
	}
}

func TestPaneRemoval(t *testing.T) {
	s := testState()

	s.CommitInventory([]*AgentState{
		{PaneID: "p1", Status: "working"},
		{PaneID: "p2", Status: "idle"},
	}, s.RevisionCounter())

	// Remove p2
	s.CommitInventory([]*AgentState{
		{PaneID: "p1", Status: "working"},
	}, s.RevisionCounter())

	if s.AgentCount() != 1 {
		t.Errorf("agent count = %d, want 1", s.AgentCount())
	}
	snap := s.Snapshot()
	if len(snap) != 1 || snap[0].PaneID != "p1" {
		t.Errorf("unexpected snapshot: %+v", snap)
	}
	if generation := s.Generation("p2"); generation != 1 {
		t.Errorf("removed pane generation = %d, want 1", generation)
	}
	s.CommitInventory([]*AgentState{
		{PaneID: "p1", Status: "working"},
		{PaneID: "p2", Status: "idle"},
	}, s.RevisionCounter())
	if generation := s.Generation("p2"); generation != 2 {
		t.Errorf("replacement pane generation = %d, want new epoch 2", generation)
	}
}

func TestSamePaneIDReplacementBumpsGeneration(t *testing.T) {
	s := testState()
	s.CommitInventory([]*AgentState{{
		PaneID: "p1", RawPaneID: "p1", TerminalID: "terminal-old",
		WorkspaceID: "workspace-old", Status: "working",
	}}, s.RevisionCounter())

	s.CommitInventory([]*AgentState{{
		PaneID: "p1", RawPaneID: "p1", TerminalID: "terminal-new",
		WorkspaceID: "workspace-new", Status: "idle",
	}}, s.RevisionCounter())

	if generation := s.Generation("p1"); generation != 1 {
		t.Fatalf("same-ID replacement generation = %d, want 1", generation)
	}
	agent, ok := s.Agent("p1")
	if !ok || agent.TerminalID != "terminal-new" || agent.WorkspaceID != "workspace-new" {
		t.Fatalf("replacement agent = %#v", agent)
	}
}

func TestFinishedNotificationOneShot(t *testing.T) {
	s := testState()

	s.CommitInventory([]*AgentState{{PaneID: "p1", Status: "idle"}}, s.RevisionCounter())

	if !s.RegisterFinishedNotification("p1") {
		t.Error("first registration should return true")
	}
	if s.RegisterFinishedNotification("p1") {
		t.Error("second registration should return false")
	}
}

func TestFinishedNotificationRejectsSupersededTransition(t *testing.T) {
	s := testState()
	s.CommitInventory([]*AgentState{{PaneID: "p1", Status: "working"}}, s.RevisionCounter())
	s.CommitEvent("p1", "idle", 1000)
	revision := s.Revision("p1")
	s.CommitEvent("p1", "working", 2000)
	if s.RegisterFinishedNotificationForTransition("p1", "idle", revision) {
		t.Fatal("superseded completion registered a notification")
	}
}

func TestInitialDoneAndDoneSynonymsDoNotPublishCompletion(t *testing.T) {
	s := testState()
	transitions := make(chan string, 2)
	s.SetOnTransition(func(_, _, _, status string, _ int64) {
		transitions <- status
	})

	s.CommitInventory([]*AgentState{{PaneID: "p1", Status: "done"}}, s.RevisionCounter())
	s.CommitEvent("p1", "completed", 1000)

	select {
	case status := <-transitions:
		t.Fatalf("false completion transition published for %q", status)
	case <-time.After(50 * time.Millisecond):
	}
	if s.RegisterFinishedNotificationForTransition("p1", "completed", s.Revision("p1")) {
		t.Fatal("non-attention completion transition registered a notification")
	}
}

func TestFinishedRegistrationRequiresAttentionTransition(t *testing.T) {
	s := testState()
	s.CommitInventory([]*AgentState{{PaneID: "p1", Status: "working"}}, s.RevisionCounter())
	s.CommitEvent("p1", "finished", 1000)
	revision := s.Revision("p1")
	if !s.RegisterFinishedNotificationForTransition("p1", "finished", revision) {
		t.Fatal("working-to-finished transition did not register")
	}
	if s.RegisterFinishedNotificationForTransition("p1", "finished", revision) {
		t.Fatal("completion transition registered twice")
	}
}

func TestCompletionRegistrationSurvivesDoneSynonymRedraw(t *testing.T) {
	s := testState()
	s.CommitInventory([]*AgentState{{PaneID: "p1", Status: "working"}}, s.RevisionCounter())
	s.CommitEvent("p1", "done", 1000)
	completionRevision := s.Revision("p1")
	s.CommitEvent("p1", "completed", 1100)
	if !s.CompletionCurrent("p1", completionRevision) {
		t.Fatal("done-synonym redraw invalidated the real completion cycle")
	}
	if !s.RegisterFinishedNotificationForTransition("p1", "done", completionRevision) {
		t.Fatal("real completion could not register after a done-synonym redraw")
	}
}

func TestRevisionTracking(t *testing.T) {
	s := testState()

	s.CommitInventory([]*AgentState{{PaneID: "p1", Status: "idle"}}, s.RevisionCounter())
	rev1 := s.Revision("p1")

	s.CommitEvent("p1", "working", 1000)
	rev2 := s.Revision("p1")

	if rev2 <= rev1 {
		t.Errorf("revision did not advance: %d -> %d", rev1, rev2)
	}
}

func TestTransitionCurrentRequiresExactStatusAndRevision(t *testing.T) {
	s := testState()
	s.CommitInventory([]*AgentState{{PaneID: "p1", Status: "working"}}, s.RevisionCounter())
	revision := s.Revision("p1")
	if !s.TransitionCurrent("p1", "working", revision) {
		t.Fatal("current transition was rejected")
	}
	s.CommitEvent("p1", "idle", 2000)
	if s.TransitionCurrent("p1", "working", revision) {
		t.Fatal("superseded transition remained current")
	}
}

func TestBlockedEventIDIsStableOnlyWithinBlockedCycle(t *testing.T) {
	s := testState()
	s.CommitInventory([]*AgentState{{PaneID: "p1", Status: "blocked"}}, s.RevisionCounter())
	first, ok := s.Agent("p1")
	if !ok || first.BlockedEventID == "" {
		t.Fatal("first blocked cycle has no event ID")
	}

	s.CommitInventory([]*AgentState{{PaneID: "p1", Status: "blocked"}}, s.RevisionCounter())
	same, _ := s.Agent("p1")
	if same.BlockedEventID != first.BlockedEventID {
		t.Fatalf("blocked event ID changed within one cycle: %q -> %q", first.BlockedEventID, same.BlockedEventID)
	}

	s.CommitEvent("p1", "working", 1000)
	working, _ := s.Agent("p1")
	if working.BlockedEventID != "" {
		t.Fatalf("non-blocked pane retained event ID %q", working.BlockedEventID)
	}
	s.CommitEvent("p1", "blocked", 2000)
	next, _ := s.Agent("p1")
	if next.BlockedEventID == "" || next.BlockedEventID == first.BlockedEventID {
		t.Fatalf("new blocked cycle event ID = %q, want a new non-empty value", next.BlockedEventID)
	}
}
