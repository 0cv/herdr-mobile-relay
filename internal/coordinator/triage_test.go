package coordinator

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTriagePersistsSeenCompletionAcrossRestart(t *testing.T) {
	cacheDir := t.TempDir()
	first := NewState(slog.Default())
	if err := first.EnableTriagePersistence(cacheDir); err != nil {
		t.Fatal(err)
	}
	identity := &AgentState{
		PaneID: "pane-1", RawPaneID: "pane-1", TerminalID: "terminal-1",
		SessionID: "session-1", Status: "working",
	}
	first.CommitInventory([]*AgentState{identity}, first.RevisionCounter())
	first.CommitEvent("pane-1", "idle", time.Now().UnixMilli()+1)
	if got := first.DisplayedStatus("pane-1"); got != "done" {
		t.Fatalf("unseen completion displayed as %q, want done", got)
	}
	if !first.AcknowledgePane("pane-1") {
		t.Fatal("completion was not acknowledged")
	}

	second := NewState(slog.Default())
	if err := second.EnableTriagePersistence(cacheDir); err != nil {
		t.Fatal(err)
	}
	second.CommitInventory([]*AgentState{{
		PaneID: "pane-1", RawPaneID: "pane-1", TerminalID: "terminal-1",
		SessionID: "session-1", Status: "idle",
	}}, second.RevisionCounter())
	if got := second.DisplayedStatus("pane-1"); got != "idle" {
		t.Fatalf("acknowledged completion after restart displayed as %q, want idle", got)
	}
	snapshot := second.Snapshot()
	if len(snapshot) != 1 || snapshot[0].LastActiveAt == 0 || snapshot[0].LastSeenAt < snapshot[0].LastActiveAt {
		t.Fatalf("durable triage timestamps were not restored: %#v", snapshot)
	}

	next := snapshot[0].LastSeenAt + 1
	second.CommitEvent("pane-1", "working", next)
	second.CommitEvent("pane-1", "idle", next+1)
	if got := second.DisplayedStatus("pane-1"); got != "done" {
		t.Fatalf("new completion after restart displayed as %q, want done", got)
	}
}

func TestTriageMigratesInferredActivityTimestamps(t *testing.T) {
	cacheDir := t.TempDir()
	identity := &AgentState{
		PaneID: "pane-1", RawPaneID: "pane-1", TerminalID: "terminal-1",
		SessionID: "session-1", Status: "done",
	}
	lastActiveAt := time.Now().Add(-time.Hour).UnixMilli()
	lastSeenAt := lastActiveAt + 1
	legacy := triageStateFile{
		Version: 1,
		Panes: map[string]triageRecord{
			triageIdentity(identity): {LastActiveAt: lastActiveAt, LastSeenAt: lastSeenAt},
		},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, triageStateFilename), data, 0o600); err != nil {
		t.Fatal(err)
	}

	state := NewState(slog.Default())
	if err := state.EnableTriagePersistence(cacheDir); err != nil {
		t.Fatal(err)
	}
	state.CommitInventory([]*AgentState{identity}, state.RevisionCounter())
	snapshot := state.Snapshot()
	if len(snapshot) != 1 || snapshot[0].LastActiveAt != 0 || snapshot[0].LastSeenAt != lastSeenAt {
		t.Fatalf("migrated triage timestamps = %#v, want unknown activity and preserved seen time", snapshot)
	}
	if got := state.DisplayedStatus("pane-1"); got != "idle" {
		t.Fatalf("acknowledged completion after migration displayed as %q, want idle", got)
	}

	stored, err := os.ReadFile(filepath.Join(cacheDir, triageStateFilename))
	if err != nil {
		t.Fatal(err)
	}
	var migrated triageStateFile
	if json.Unmarshal(stored, &migrated) != nil || migrated.Version != triageStateVersion {
		t.Fatalf("migrated triage file = %s", stored)
	}
}

func TestTriageDoesNotCrossSessionIdentity(t *testing.T) {
	cacheDir := t.TempDir()
	first := NewState(slog.Default())
	if err := first.EnableTriagePersistence(cacheDir); err != nil {
		t.Fatal(err)
	}
	first.CommitInventory([]*AgentState{{
		PaneID: "pane-1", RawPaneID: "pane-1", TerminalID: "terminal-1",
		SessionID: "session-old", Status: "done",
	}}, first.RevisionCounter())
	first.AcknowledgePane("pane-1")

	second := NewState(slog.Default())
	if err := second.EnableTriagePersistence(cacheDir); err != nil {
		t.Fatal(err)
	}
	second.CommitInventory([]*AgentState{{
		PaneID: "pane-1", RawPaneID: "pane-1", TerminalID: "terminal-1",
		SessionID: "session-new", Status: "done",
	}}, second.RevisionCounter())
	if got := second.DisplayedStatus("pane-1"); got != "done" {
		t.Fatalf("replacement session inherited seen state: displayed %q", got)
	}
}
