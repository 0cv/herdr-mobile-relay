package coordinator

import (
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/question"
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
	if status["state"] != "error" || status["stale"] != true ||
		status["error_code"] != "command_failed" || status["message"] != "Unable to read the current Herdr agent inventory." {
		t.Fatalf("inventory status = %+v, want generic error with stale inventory", status)
	}
	if state.AgentCount() != 1 {
		t.Fatal("failed poll discarded the last known snapshot")
	}
}

func TestInventoryFailureReportsServerNotRunning(t *testing.T) {
	state := testState()
	state.MarkInventoryFailure(&herdr.CLIError{
		Code:    "server_not_running",
		Message: "no Herdr server",
	})
	status := state.InventoryStatus()
	if status["error_code"] != "server_not_running" ||
		status["message"] != "Herdr is not running on this computer. Start it with `herdr`." {
		t.Fatalf("inventory status = %+v, want actionable server-not-running message", status)
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
	// The backing store is a map; the snapshot must impose a stable order so
	// consecutive identical inventories serialize identically.
	if snap[0].PaneID != "p1" || snap[1].PaneID != "p2" {
		t.Errorf("snapshot order = %s, %s, want p1, p2", snap[0].PaneID, snap[1].PaneID)
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
	if reappeared.LastActiveAt != 0 {
		t.Fatalf("reappeared last_active_at = %d, want unknown without observed activity", reappeared.LastActiveAt)
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
	if updated.LastActiveAt <= 0 {
		t.Fatalf("activity last_active_at = %d, want epoch milliseconds", updated.LastActiveAt)
	}
}

func TestInventoryMetadataDoesNotAdvanceLastActiveAt(t *testing.T) {
	s := testState()
	s.CommitInventory([]*AgentState{{
		PaneID: "p1", Status: "idle", Name: "old",
	}}, s.RevisionCounter())

	s.CommitInventory([]*AgentState{{
		PaneID: "p1", Status: "idle", Name: "new",
	}}, s.RevisionCounter())
	updated, _ := s.Agent("p1")
	if updated.UpdatedAt <= 0 {
		t.Fatalf("metadata updated_at = %d, want epoch milliseconds", updated.UpdatedAt)
	}
	if updated.LastActiveAt != 0 {
		t.Fatalf("metadata last_active_at = %d, want unknown without observed activity", updated.LastActiveAt)
	}
}

func TestStatusClockTracksObservedSpells(t *testing.T) {
	s := testState()
	pane := &AgentState{PaneID: "p1", TerminalID: "terminal", Status: "idle"}
	poll := func(status string) {
		pane.Status = status
		s.CommitInventory([]*AgentState{pane}, s.RevisionCounter())
	}
	check := func(status string, since int64) {
		t.Helper()
		a := s.Snapshot()[0]
		if a.Status != status || a.StatusSinceAt != since {
			t.Fatalf("displayed %q since %d, want %q since %d", a.Status, a.StatusSinceAt, status, since)
		}
	}

	poll("working")
	check("working", 0)
	poll("idle")
	complete := s.lastAttemptAt.UnixMilli()
	check("done", complete)
	s.AcknowledgePane("p1")
	check("idle", 0)

	poll("working")
	started := s.lastAttemptAt.UnixMilli()
	check("working", started)
	pane.Name = "renamed"
	pane.ActivitySeq++
	poll("working")
	check("working", started)
	s.CommitEvent("p1", "working", started+1)
	check("working", started)

	s.CommitEvent("p1", "blocked", started+2)
	check("blocked", started+2)
	pane.Prompt = "Approve?"
	poll("blocked")
	check("blocked", started+2)
	s.CommitEvent("p1", "succeeded", started+3)
	check("succeeded", started+3)
	poll("idle")
	check("done", started+3)
	poll("completed")
	check("completed", started+3)

	s.AcknowledgePane("p1")
	check("idle", 0)
	poll("completed")
	check("idle", 0)
	s.CommitEvent("p1", "working", started+4)
	check("working", started+4)
	s.CommitEvent("p1", "idle", started+5)
	check("done", started+5)
	s.CommitEvent("p1", "working", started+6)
	check("working", started+6)
}

func TestStatusClockStartsWhenChatBecomesAWait(t *testing.T) {
	for _, source := range []string{"poll", "classification"} {
		t.Run(source, func(t *testing.T) {
			s := testState()
			pane := &AgentState{PaneID: "p1", TerminalID: "terminal", Status: "working"}
			poll := func() { s.CommitInventory([]*AgentState{pane}, s.RevisionCounter()) }
			poll()
			pane.Status = "blocked"
			pane.AttentionKind = question.AttentionChat
			poll()
			if since := s.Snapshot()[0].StatusSinceAt; since != 0 {
				t.Fatalf("idle chat has a clock: %d", since)
			}

			setKind := func(kind question.AttentionKind) {
				t.Helper()
				if source == "poll" {
					pane.AttentionKind = kind
					poll()
					return
				}
				agent, _ := s.Agent(pane.PaneID)
				if _, _, ok := s.CommitAttentionClassification(
					pane.PaneID, agent.BlockedEventID, agent.Generation, s.ContentRevision(pane.PaneID),
					question.Classification{Kind: kind}, "",
				); !ok {
					t.Fatal("classification rejected")
				}
			}

			before := time.Now().UnixMilli()
			setKind(question.AttentionApproval)
			started := s.Snapshot()[0].StatusSinceAt
			if started < before || started > time.Now().UnixMilli() {
				t.Fatalf("waiting clock = %d, want time of approval classification", started)
			}
			setKind(question.AttentionQuestion)
			if since := s.Snapshot()[0].StatusSinceAt; since != started {
				t.Fatalf("same displayed state restarted the clock: %d, want %d", since, started)
			}
			setKind(question.AttentionChat)
			if since := s.Snapshot()[0].StatusSinceAt; since != 0 {
				t.Fatalf("idle chat retained the waiting clock: %d", since)
			}
		})
	}
}

func TestStatusClockKeepsOneStartAcrossWorkingAliases(t *testing.T) {
	s := testState()
	s.CommitInventory([]*AgentState{{PaneID: "p1", Status: "idle"}}, 0)
	for i, status := range []string{"working", "busy", "working"} {
		s.CommitEvent("p1", status, 100+int64(i))
		a := s.Snapshot()[0]
		if a.Status != status || a.StatusSinceAt != 100 {
			t.Fatalf("displayed %q since %d, want %q since 100", a.Status, a.StatusSinceAt, status)
		}
	}
}

func TestStatusClockDoesNotCrossTargets(t *testing.T) {
	for _, change := range []string{"terminal", "session", "generation"} {
		t.Run(change, func(t *testing.T) {
			s := testState()
			pane := &AgentState{PaneID: "p1", TerminalID: "terminal", SessionID: "session", Status: "idle"}
			s.CommitInventory([]*AgentState{pane}, 0)
			s.CommitEvent("p1", "working", 100)
			if change == "session" {
				s.CommitEvent("p1", "idle", 150)
			}
			pane.Status = "working"
			switch change {
			case "terminal":
				pane.TerminalID = "new-terminal"
			case "session":
				pane.SessionID = "new-session"
			case "generation":
				s.BumpGeneration("p1")
			}
			s.CommitInventory([]*AgentState{pane}, s.RevisionCounter())
			a := s.Snapshot()[0]
			if a.Status != "working" || a.StatusSinceAt != 0 {
				t.Fatalf("replacement = %q since %d, want working without a clock", a.Status, a.StatusSinceAt)
			}
		})
	}
}

func TestStatusClockUsesReplacementTopologySample(t *testing.T) {
	s := testState()
	pane := &AgentState{PaneID: "p1", TerminalID: "terminal", SessionID: "session-a", Agent: "codex", Status: "idle"}
	s.CommitInventory([]*AgentState{pane}, 0)
	pane.SessionID = "session-b"
	s.CommitTopology([]*AgentState{pane}, nil, s.RevisionCounter())
	if a := s.Snapshot()[0]; a.SessionID != "session-b" || a.Status != "idle" || a.StatusSinceAt != 0 {
		t.Fatalf("topology replacement = %q %q since %d, want session-b idle without a clock", a.SessionID, a.Status, a.StatusSinceAt)
	}

	s.CommitEvent("p1", "working", 100)
	if a := s.Snapshot()[0]; a.Status != "working" || a.StatusSinceAt != 100 {
		t.Fatalf("displayed %q since %d, want working since 100", a.Status, a.StatusSinceAt)
	}
}

func TestStatusClockUsesDisplayedResultAsReplacementBaseline(t *testing.T) {
	s := testState()
	pane := &AgentState{PaneID: "p1", TerminalID: "terminal", SessionID: "session-a", Agent: "codex", Status: "working"}
	s.CommitInventory([]*AgentState{pane}, 0)
	s.CommitEvent("p1", "idle", 100)
	if a := s.Snapshot()[0]; a.Status != "done" || a.StatusSinceAt != 100 {
		t.Fatalf("original completion = %q since %d, want done since 100", a.Status, a.StatusSinceAt)
	}

	pane.SessionID = "session-b"
	pane.Status = "idle"
	s.CommitTopology([]*AgentState{pane}, nil, s.RevisionCounter())
	if a := s.Snapshot()[0]; a.SessionID != "session-b" || a.Status != "done" || a.StatusSinceAt != 0 {
		t.Fatalf("replacement topology = %q %q since %d, want session-b done without a clock", a.SessionID, a.Status, a.StatusSinceAt)
	}
	s.CommitInventory([]*AgentState{pane}, s.RevisionCounter())
	if a := s.Snapshot()[0]; a.Status != "done" || a.StatusSinceAt != 0 {
		t.Fatalf("unchanged replacement poll = %q since %d, want done without a clock", a.Status, a.StatusSinceAt)
	}

	pane.Status = "working"
	s.CommitInventory([]*AgentState{pane}, s.RevisionCounter())
	if a := s.Snapshot()[0]; a.Status != "working" || a.StatusSinceAt != s.lastAttemptAt.UnixMilli() {
		t.Fatalf("replacement started working = %q since %d, want working since %d", a.Status, a.StatusSinceAt, s.lastAttemptAt.UnixMilli())
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

func TestAcknowledgeExplicitDonePersistsAcrossSnapshots(t *testing.T) {
	s := testState()
	s.CommitInventory([]*AgentState{{PaneID: "p1", Agent: "codex", Status: "working"}}, s.RevisionCounter())
	s.CommitInventory([]*AgentState{{PaneID: "p1", Agent: "codex", Status: "done"}}, s.RevisionCounter())

	if _, ok := s.AcknowledgePane("p1"); !ok {
		t.Fatal("explicit done pane was not acknowledged")
	}
	if got := s.DisplayedStatus("p1"); got != "idle" {
		t.Fatalf("displayed after explicit done acknowledgement = %q, want idle", got)
	}
	if snapshot := s.Snapshot(); len(snapshot) != 1 || snapshot[0].Status != "idle" {
		t.Fatalf("snapshot after explicit done acknowledgement = %#v, want one idle pane", snapshot)
	}

	s.CommitInventory([]*AgentState{{PaneID: "p1", Agent: "codex", Status: "done"}}, s.RevisionCounter())
	if got := s.DisplayedStatus("p1"); got != "idle" {
		t.Fatalf("repeated explicit done snapshot restored %q, want idle", got)
	}

	s.CommitInventory([]*AgentState{{PaneID: "p1", Agent: "codex", Status: "working"}}, s.RevisionCounter())
	s.CommitInventory([]*AgentState{{PaneID: "p1", Agent: "codex", Status: "done"}}, s.RevisionCounter())
	if got := s.DisplayedStatus("p1"); got != "done" {
		t.Fatalf("new completion after resumed work displayed %q, want done", got)
	}

	initialDone := testState()
	initialDone.CommitInventory([]*AgentState{{PaneID: "p2", Agent: "codex", Status: "done"}}, initialDone.RevisionCounter())
	initialDone.AcknowledgePane("p2")
	if got := initialDone.DisplayedStatus("p2"); got != "idle" {
		t.Fatalf("acknowledged initial explicit done displayed %q, want idle", got)
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
	s.CommitInventory([]*AgentState{{PaneID: "p1", Status: "working"}}, s.RevisionCounter())
	s.CommitEvent("p1", "idle", 1000)
	revision := s.Revision("p1")

	if !s.RegisterFinishedNotificationForTransition("p1", revision) {
		t.Error("first registration should return true")
	}
	if s.RegisterFinishedNotificationForTransition("p1", revision) {
		t.Error("second registration should return false")
	}
}

func TestFinishedNotificationRejectsSupersededTransition(t *testing.T) {
	s := testState()
	s.CommitInventory([]*AgentState{{PaneID: "p1", Status: "working"}}, s.RevisionCounter())
	s.CommitEvent("p1", "idle", 1000)
	revision := s.Revision("p1")
	s.CommitEvent("p1", "working", 2000)
	if s.RegisterFinishedNotificationForTransition("p1", revision) {
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
	if s.RegisterFinishedNotificationForTransition("p1", s.Revision("p1")) {
		t.Fatal("non-attention completion transition registered a notification")
	}
}

func TestWorkingTransitionCurrentSurvivesReportsButNotGenerationChanges(t *testing.T) {
	for _, change := range []string{"explicit bump", "session change", "pane replacement"} {
		t.Run(change, func(t *testing.T) {
			s := testState()
			pane := &AgentState{PaneID: "p1", TerminalID: "terminal-1", Status: "blocked"}
			poll := func() { s.CommitInventory([]*AgentState{pane}, s.RevisionCounter()) }
			poll()
			if !s.CommitEvent(pane.PaneID, "working", 1) {
				t.Fatal("missing working transition")
			}
			workingRevision := s.Revision(pane.PaneID)
			s.CommitEvent(pane.PaneID, "working", 2)
			pane.Status = "working"
			poll()
			if !s.WorkingTransitionCurrent(pane.PaneID, workingRevision) {
				t.Fatal("repeat working reports invalidated the transition")
			}
			switch change {
			case "explicit bump":
				s.BumpGeneration(pane.PaneID)
			case "session change":
				pane.SessionID = "session-1"
				poll()
			case "pane replacement":
				pane.TerminalID = "terminal-2"
				poll()
			}
			if s.WorkingTransitionCurrent(pane.PaneID, workingRevision) {
				t.Fatal("old working transition survived a new pane generation")
			}
		})
	}
}

func TestFinishedRegistrationRequiresAttentionTransition(t *testing.T) {
	s := testState()
	s.CommitInventory([]*AgentState{{PaneID: "p1", Status: "working"}}, s.RevisionCounter())
	s.CommitEvent("p1", "finished", 1000)
	revision := s.Revision("p1")
	if !s.RegisterFinishedNotificationForTransition("p1", revision) {
		t.Fatal("working-to-finished transition did not register")
	}
	if s.RegisterFinishedNotificationForTransition("p1", revision) {
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
	if !s.RegisterFinishedNotificationForTransition("p1", completionRevision) {
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

func TestBlockedClassificationStartsUnknownAndCommitsUnderEventGuards(t *testing.T) {
	s := NewState(testLogger())
	s.CommitInventory([]*AgentState{{PaneID: "p1", Agent: "codex", Status: "working"}}, 0)
	s.CommitEvent("p1", "blocked", 1000)
	blocked, _ := s.Agent("p1")
	if blocked.AttentionKind != question.AttentionUnknown ||
		len(blocked.Options) != 0 || blocked.Interaction != nil {
		t.Fatalf("initial blocked state = %+v, want unknown without controls", blocked)
	}
	generation := s.Generation("p1")
	classification := question.Classification{
		Kind:    question.AttentionApproval,
		Command: "make check",
		Options: []string{"Approve", "Reject"},
	}
	current, _, ok := s.CommitAttentionClassification(
		"p1", blocked.BlockedEventID, generation, s.ContentRevision("p1"), classification, "",
	)
	if !ok || current.AttentionKind != question.AttentionApproval ||
		len(current.Options) != 2 || current.StateRevision == blocked.StateRevision {
		t.Fatalf("committed classification = %+v, ok=%v", current, ok)
	}

	s.CommitEvent("p1", "working", 2000)
	if _, _, ok := s.CommitAttentionClassification(
		"p1", blocked.BlockedEventID, generation, s.ContentRevision("p1"), classification, "",
	); ok {
		t.Fatal("stale blocked classification committed after the status changed")
	}
	working, _ := s.Agent("p1")
	if working.AttentionKind != "" || len(working.Options) != 0 ||
		working.Interaction != nil || working.Command != "" {
		t.Fatalf("working state retained blocked controls: %+v", working)
	}
}

func TestInflightPollPreservesNewerBlockedClassification(t *testing.T) {
	s := NewState(testLogger())
	s.CommitInventory([]*AgentState{{PaneID: "p1", Agent: "codex", Status: "working"}}, 0)
	token := s.BeginPoll()
	s.CommitEvent("p1", "blocked", 1000)
	blocked, _ := s.Agent("p1")
	if _, _, ok := s.CommitAttentionClassification(
		"p1",
		blocked.BlockedEventID,
		s.Generation("p1"),
		s.ContentRevision("p1"),
		question.Classification{
			Kind:    question.AttentionApproval,
			Command: "make check",
			Options: []string{"Approve", "Reject"},
		}, "",
	); !ok {
		t.Fatal("classification setup failed")
	}

	if _, committed := s.CommitPoll([]*AgentState{{
		PaneID: "p1", Agent: "codex", Status: "working",
	}}, nil, token); !committed {
		t.Fatal("stable in-flight poll was rejected")
	}
	current, _ := s.Agent("p1")
	if current.Status != "blocked" ||
		current.AttentionKind != question.AttentionApproval ||
		len(current.Options) != 2 {
		t.Fatalf("in-flight poll overwrote newer classification: %+v", current)
	}
}

func TestTopologyCommitPreservesCurrentStatusAndAttention(t *testing.T) {
	s := NewState(testLogger())
	s.CommitInventory([]*AgentState{{PaneID: "p1", Agent: "codex", Status: "working"}}, 0)
	s.CommitEvent("p1", "blocked", 1000)
	blocked, _ := s.Agent("p1")
	if _, _, ok := s.CommitAttentionClassification(
		"p1",
		blocked.BlockedEventID,
		s.Generation("p1"),
		s.ContentRevision("p1"),
		question.Classification{
			Kind:    question.AttentionApproval,
			Options: []string{"Approve", "Reject"},
		}, "",
	); !ok {
		t.Fatal("classification setup failed")
	}

	s.CommitTopology([]*AgentState{{
		PaneID: "p1",
		Agent:  "codex",
		Name:   "renamed",
		Status: "idle",
	}}, nil, s.RevisionCounter())
	current, _ := s.Agent("p1")
	if current.Status != "blocked" ||
		current.AttentionKind != question.AttentionApproval ||
		len(current.Options) != 2 ||
		current.Name != "renamed" {
		t.Fatalf("topology commit overwrote current state: %+v", current)
	}
}

func TestInflightPollCannotOverwriteNewerWorkspaceEvent(t *testing.T) {
	s := NewState(testLogger())
	s.CommitInventory([]*AgentState{{PaneID: "p1", Agent: "codex", Status: "working"}}, 0)
	s.CommitWorkspaces([]herdr.Workspace{{ID: "w1", Label: "Before"}})
	token := s.BeginPoll()

	s.CommitTopology(
		[]*AgentState{{PaneID: "p1", Agent: "codex", Status: "working"}},
		[]herdr.Workspace{{ID: "w1", Label: "Desktop rename"}},
		s.RevisionCounter(),
	)
	if _, committed := s.CommitPoll(
		[]*AgentState{{PaneID: "p1", Agent: "codex", Status: "working"}},
		[]herdr.Workspace{{ID: "w1", Label: "Before"}},
		token,
	); committed {
		t.Fatal("poll sampled before the workspace event was committed")
	}
	workspace, ok := s.Workspace("w1")
	if !ok || workspace.Label != "Desktop rename" {
		t.Fatalf("workspace = %+v, ok=%v", workspace, ok)
	}
}

func TestAttentionRevisionIgnoresUnrelatedPollChanges(t *testing.T) {
	s := NewState(testLogger())
	transitions := 0
	s.SetOnTransition(func(_, _, _, _ string, _ int64) {
		transitions++
	})
	s.CommitInventory([]*AgentState{{
		PaneID: "p1", Agent: "codex", Status: "blocked",
		AttentionKind: question.AttentionApproval,
		Options:       []string{"Approve", "Reject"},
		ActivitySeq:   1,
	}}, 0)
	blocked, _ := s.Agent("p1")
	generation := s.Generation("p1")
	attentionRevision := s.AttentionRevision("p1")

	s.CommitInventory([]*AgentState{{
		PaneID: "p1", Agent: "codex", Status: "blocked",
		AttentionKind: question.AttentionApproval,
		Options:       []string{"Approve", "Reject"},
		ActivitySeq:   2,
	}}, s.RevisionCounter())
	if !s.AttentionTransitionCurrent(
		"p1",
		blocked.BlockedEventID,
		generation,
		string(question.AttentionApproval),
		attentionRevision,
	) {
		t.Fatal("unrelated activity sequence invalidated the approval classification")
	}
	if transitions != 1 {
		t.Fatalf("unrelated poll changes emitted %d transitions, want 1", transitions)
	}

	s.CommitInventory([]*AgentState{{
		PaneID: "p1", Agent: "codex", Status: "blocked",
		AttentionKind: question.AttentionApproval,
		Options:       []string{"Approve once", "Reject"},
		ActivitySeq:   2,
	}}, s.RevisionCounter())
	if s.AttentionTransitionCurrent(
		"p1",
		blocked.BlockedEventID,
		generation,
		string(question.AttentionApproval),
		attentionRevision,
	) {
		t.Fatal("changed approval controls did not invalidate the classification")
	}
	if transitions != 2 {
		t.Fatalf("changed approval controls emitted %d transitions, want 2", transitions)
	}
}

func TestBlockedClassificationChangeTriggersAndChatSuppressesIdleCompletion(t *testing.T) {
	s := NewState(testLogger())
	var transitions []string
	s.SetOnTransition(func(_ string, _ string, _ string, status string, _ int64) {
		transitions = append(transitions, status)
	})
	s.CommitInventory([]*AgentState{{
		PaneID: "p1", Agent: "codex", Status: "blocked",
		AttentionKind: question.AttentionApproval,
		Options:       []string{"Approve", "Reject"},
	}}, 0)
	if len(transitions) != 1 {
		t.Fatalf("initial transitions = %v, want blocked", transitions)
	}
	s.CommitInventory([]*AgentState{{
		PaneID: "p1", Agent: "codex", Status: "blocked",
		AttentionKind: question.AttentionChat,
	}}, s.RevisionCounter())
	if len(transitions) != 2 {
		t.Fatalf("classification transitions = %v, want blocked reclassification", transitions)
	}
	chat, _ := s.Agent("p1")
	if !s.CompletionCurrent("p1", chat.StateRevision) {
		t.Fatal("chat classification did not establish a completion transition")
	}
	if !s.RegisterFinishedNotificationForTransition("p1", chat.StateRevision) {
		t.Fatal("chat completion could not register its notification")
	}

	s.CommitEvent("p1", "idle", 3000)
	if len(transitions) != 2 {
		t.Fatalf("raw idle duplicated chat completion: transitions = %v", transitions)
	}
	if !s.CompletionCurrent("p1", chat.StateRevision) {
		t.Fatal("raw idle canceled the classified chat completion")
	}
}

func TestCustomAnswerMemoryNormalizesAndPrunes(t *testing.T) {
	s := NewState(testLogger())
	s.CommitInventory([]*AgentState{{PaneID: "pane-1", Status: "blocked"}}, s.RevisionCounter())

	s.RecordCustomAnswer("pane-1", "Roughly how much TIME do you want?", "Tyy")
	s.RecordCustomAnswer("pane-1", "", "ignored")
	s.RecordCustomAnswer("pane-1", "Empty answer?", "   ")
	answers := s.CustomAnswers("pane-1")
	if len(answers) != 1 ||
		answers[question.SummaryKey("roughly how much time do you want")] != "Tyy" {
		t.Fatalf("answers = %#v", answers)
	}

	s.CommitInventory([]*AgentState{{PaneID: "pane-2", Status: "working"}}, s.RevisionCounter())
	if remaining := s.CustomAnswers("pane-1"); remaining != nil {
		t.Fatalf("answers survived pane removal: %#v", remaining)
	}
}
