package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/activity"
	"github.com/0cv/herdr-mobile-relay/internal/conversation"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
)

func TestActivityBackfillKeepsHistoricalSessionCwdEmpty(t *testing.T) {
	home := t.TempDir()
	const sessionID = "123e4567-e89b-12d3-a456-426614174399"
	writeClaudeTranscriptAnswering(t,
		filepath.Join(home, ".claude", "projects", "-work-old", sessionID+".jsonl"),
		"Old work", "historical answer")

	s := testServer()
	s.conversationM = conversation.NewReader(home)
	s.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", Agent: "claude", Cwd: "/work/new", ForegroundCwd: "/work/unrelated",
	}}, s.state.RevisionCounter())
	s.activityView = []activity.Entry{{
		ID:        "historical-finished",
		Timestamp: activity.MilliTimestamp(time.Date(2026, time.August, 12, 10, 0, 2, 0, time.UTC).UnixMilli()),
		Kind:      "finished",
		Status:    "completed",
		Agent:     "claude",
		PaneID:    "pane-1",
		Session:   sessionID,
	}}

	backfilled := s.recentActivities(1)
	if len(backfilled) != 1 || backfilled[0].Extract != "historical answer" {
		t.Fatalf("historical activity = %#v, want response located without current pane cwd", backfilled)
	}
}

func TestActivityBackfillUsesLiveForegroundProject(t *testing.T) {
	home := t.TempDir()
	const sessionID = "123e4567-e89b-12d3-a456-426614174398"
	writeClaudeTranscriptAnswering(t,
		filepath.Join(home, ".claude", "projects", "-work-pane", sessionID+".jsonl"),
		"Pane copy", "pane answer")
	writeClaudeTranscriptAnswering(t,
		filepath.Join(home, ".claude", "projects", "-work-foreground", sessionID+".jsonl"),
		"Foreground copy", "foreground answer")

	s := testServer()
	s.conversationM = conversation.NewReader(home)
	s.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", Agent: "claude", Cwd: "/work/pane", ForegroundCwd: "/work/foreground", SessionID: sessionID,
	}}, s.state.RevisionCounter())
	s.activityView = []activity.Entry{{
		ID: "live-finished", Kind: "finished", Status: "completed", Agent: "claude", PaneID: "pane-1", Session: sessionID,
	}}

	backfilled := s.recentActivities(1)
	if len(backfilled) != 1 || backfilled[0].Extract != "foreground answer" {
		t.Fatalf("live foreground activity = %#v, want foreground response", backfilled)
	}
}

func TestCommittedActivityViewTracksLiveCommitAndClear(t *testing.T) {
	s := testServer()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.hub.Shutdown(ctx)
	})
	entry := activity.NewEntry("prompt", "sent", "hello", "pane-1", "", "", "request-1")
	s.broadcastCommitted(map[string]any{"type": "activity", "activity": entry})
	recent := s.recentActivities(500)
	if len(recent) != 1 || recent[0].ID != entry.ID {
		t.Fatalf("committed view = %+v", recent)
	}
	s.broadcastCommitted(map[string]any{"type": "activity_history", "activities": []activity.Entry{}})
	if recent := s.recentActivities(500); len(recent) != 0 {
		t.Fatalf("cleared view = %+v", recent)
	}
}
