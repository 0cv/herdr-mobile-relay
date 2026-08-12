package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/activity"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
)

func TestObservedWorkingTransitionRemainsInActivityAfterStateAdvances(t *testing.T) {
	s := testServer()
	journal, err := activity.OpenJournal(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	s.dispatcher = coordinator.NewDispatcher(nil, s.state, journal, s.logger)
	t.Cleanup(func() {
		_ = s.dispatcher.Close(context.Background())
	})

	s.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", Agent: "codex", Project: "relay", Status: "working",
	}}, s.state.RevisionCounter())
	workingRevision := s.state.Revision("pane-1")
	observedAt := time.Now().Add(-5 * time.Minute).UnixMilli()
	s.state.CommitEvent("pane-1", "blocked", time.Now().UnixMilli())

	s.handleTransition(
		context.Background(), "pane-1", "codex", "relay", "working",
		workingRevision, observedAt,
	)

	entries := journal.Recent(10)
	if len(entries) != 1 || entries[0].Kind != "working" || int64(entries[0].Timestamp) != observedAt {
		t.Fatalf("working activity = %#v", entries)
	}
}
