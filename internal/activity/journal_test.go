package activity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppendAndRecent(t *testing.T) {
	dir := t.TempDir()
	j, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}

	e1 := NewEntry("prompt", "sent", "test prompt", "p1", "claude", "proj", "r1")
	e2 := NewEntry("approval", "approved", "approved action", "p1", "claude", "proj", "r2")

	if err := j.Append(e1); err != nil {
		t.Fatal(err)
	}
	if err := j.Append(e2); err != nil {
		t.Fatal(err)
	}

	recent := j.Recent(10)
	if len(recent) != 2 {
		t.Fatalf("recent len = %d, want 2", len(recent))
	}
	if recent[0].Summary != "test prompt" {
		t.Errorf("first entry summary = %q", recent[0].Summary)
	}
	if recent[1].Summary != "approved action" {
		t.Errorf("second entry summary = %q", recent[1].Summary)
	}
}

func TestPersistenceAcrossRestart(t *testing.T) {
	dir := t.TempDir()

	j1, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	j1.Append(NewEntry("prompt", "sent", "persisted", "p1", "claude", "", ""))

	// Reopen
	j2, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	recent := j2.Recent(10)
	if len(recent) != 1 {
		t.Fatalf("after reopen, len = %d, want 1", len(recent))
	}
	if recent[0].Summary != "persisted" {
		t.Errorf("summary = %q, want persisted", recent[0].Summary)
	}
}

func TestClear(t *testing.T) {
	dir := t.TempDir()
	j, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}

	j.Append(NewEntry("prompt", "sent", "to clear", "p1", "", "", ""))
	if err := j.Clear(); err != nil {
		t.Fatal(err)
	}

	if len(j.Recent(10)) != 0 {
		t.Error("expected empty journal after clear")
	}

	// Verify persisted
	j2, _ := OpenJournal(dir)
	if len(j2.Recent(10)) != 0 {
		t.Error("expected empty journal after reopen")
	}
}

func TestMaxItemsEnforced(t *testing.T) {
	dir := t.TempDir()
	j, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 600; i++ {
		j.Append(NewEntry("test", "ok", "entry", "p1", "", "", ""))
	}

	if len(j.Recent(0)) != maxItems {
		t.Errorf("entries = %d, want %d", len(j.Recent(0)), maxItems)
	}
}

func TestExtractTruncation(t *testing.T) {
	dir := t.TempDir()
	j, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}

	longExtract := make([]byte, maxExtractChars+1000)
	for i := range longExtract {
		longExtract[i] = 'x'
	}

	e := NewEntry("blocked", "blocked", "test", "p1", "", "", "")
	e.Extract = string(longExtract)
	j.Append(e)

	recent := j.Recent(1)
	if len(recent[0].Extract) != maxExtractChars {
		t.Errorf("extract len = %d, want %d", len(recent[0].Extract), maxExtractChars)
	}
}

func TestJournalFileCreated(t *testing.T) {
	dir := t.TempDir()
	j, _ := OpenJournal(dir)
	j.Append(NewEntry("test", "ok", "file check", "", "", "", ""))

	path := filepath.Join(dir, "activity.jsonl")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("journal file not created: %v", err)
	}
}

func TestDiscardPersistsWithoutLeavingTombstone(t *testing.T) {
	dir := t.TempDir()
	journal, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	discarded := NewEntry("blocked", "attention", "stale", "p1", "", "", "")
	retained := NewEntry("finished", "completed", "current", "p1", "", "", "")
	if err := journal.Append(discarded); err != nil {
		t.Fatal(err)
	}
	if err := journal.Append(retained); err != nil {
		t.Fatal(err)
	}
	if err := journal.Discard(discarded.ID); err != nil {
		t.Fatal(err)
	}
	recent := journal.Recent(10)
	if len(recent) != 1 || recent[0].ID != retained.ID {
		t.Fatalf("recent = %+v, want only retained entry", recent)
	}
	if _, err := os.Stat(filepath.Join(dir, "activity.tombstones")); !os.IsNotExist(err) {
		t.Fatalf("tombstone cleanup error = %v, want file absent", err)
	}
	reopened, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	if recent := reopened.Recent(10); len(recent) != 1 || recent[0].ID != retained.ID {
		t.Fatalf("reopened recent = %+v, want only retained entry", recent)
	}
}

func TestLegacyTombstoneRecoveryFiltersStaleEntry(t *testing.T) {
	dir := t.TempDir()
	stale := NewEntry("blocked", "attention", "stale", "p1", "", "", "")
	current := NewEntry("finished", "completed", "current", "p1", "", "", "")
	writeJournalFixture(t, dir, stale, current)
	if err := os.WriteFile(
		filepath.Join(dir, "activity.tombstones"),
		[]byte(stale.ID+"\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	journal, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	recent := journal.Recent(10)
	if len(recent) != 1 || recent[0].ID != current.ID {
		t.Fatalf("recovered recent = %+v, want only current entry", recent)
	}
	if _, err := os.Stat(filepath.Join(dir, "activity.tombstones")); !os.IsNotExist(err) {
		t.Fatalf("recovered tombstone cleanup error = %v, want file absent", err)
	}
}

func TestClearCrashWindowRecoversAsEmpty(t *testing.T) {
	dir := t.TempDir()
	first := NewEntry("prompt", "sent", "first", "p1", "", "", "")
	second := NewEntry("approval", "approved", "second", "p1", "", "", "")
	writeJournalFixture(t, dir, first, second)
	tombstones := strings.Join([]string{first.ID, second.ID, ""}, "\n")
	if err := os.WriteFile(
		filepath.Join(dir, "activity.tombstones"),
		[]byte(tombstones),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	journal, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	if recent := journal.Recent(10); len(recent) != 0 {
		t.Fatalf("recovered clear has %d entries, want 0", len(recent))
	}
	data, err := os.ReadFile(filepath.Join(dir, "activity.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 0 {
		t.Fatalf("recovered clear journal = %q, want empty", data)
	}
}

func TestCorruptJournalSkipsBadLines(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(dir, "activity.jsonl"),
		[]byte("{not-json}\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	j, err := OpenJournal(dir)
	if err != nil {
		t.Fatalf("OpenJournal error = %v, want nil (corrupt lines are skipped)", err)
	}
	if entries := j.Recent(10); len(entries) != 0 {
		t.Fatalf("expected 0 entries after skipping corrupt line, got %d", len(entries))
	}
}

func writeJournalFixture(t *testing.T, dir string, entries ...Entry) {
	t.Helper()
	var data []byte
	for _, entry := range entries {
		line, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, line...)
		data = append(data, '\n')
	}
	if err := os.WriteFile(filepath.Join(dir, "activity.jsonl"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}
