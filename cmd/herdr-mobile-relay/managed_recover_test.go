package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/managedstate"
)

// stageRecoverJournal leaves a durable staged S7A journal with no live owner
// by staging it through a T-only transaction and releasing the lock.
func stageRecoverJournal(t *testing.T, dir, runID string, entries []managedstate.JournalEntry) {
	t.Helper()
	root, err := managedstate.OpenExistingRoot(dir)
	if err != nil {
		t.Fatalf("OpenExistingRoot: %v", err)
	}
	txn, err := root.AcquireTransaction(context.Background(), 0)
	if err != nil {
		t.Fatalf("AcquireTransaction: %v", err)
	}
	if _, err := txn.BeginJournal(runID, runID, entries); err != nil {
		t.Fatalf("BeginJournal: %v", err)
	}
	if err := txn.Release(); err != nil {
		t.Fatalf("release staged transaction: %v", err)
	}
	if err := root.Close(); err != nil {
		t.Fatalf("close staged root: %v", err)
	}
}

func recoverJournalPresent(t *testing.T, dir string) bool {
	t.Helper()
	info, err := os.Lstat(filepath.Join(dir, "journal.json"))
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		t.Fatalf("lstat journal.json: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("journal.json has mode %v, want regular file", info.Mode())
	}
	return true
}

func assertNoRecoverJournal(t *testing.T, dir string) {
	t.Helper()
	if recoverJournalPresent(t, dir) {
		t.Fatal("journal.json still present after recovery")
	}
	matches, err := filepath.Glob(filepath.Join(dir, "journal.stage.*"))
	if err != nil {
		t.Fatalf("glob staged journal files: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("staged journal files still present: %v", matches)
	}
}

// TestManagedStateRecoverUsageAndOutcome covers the strict flag contract and
// the staged success/no-op JSON lines.
func TestManagedStateRecoverUsageAndOutcome(t *testing.T) {
	dir := managedStateRoot(t)
	stageRecoverJournal(t, dir, "run-recover", []managedstate.JournalEntry{
		{Path: "phone-app-origin-configured", PriorExists: false, PriorMode: 0o600, NewBytes: []byte("https://new.example"), NewMode: 0o600},
	})

	usageCases := []struct {
		name string
		args []string
	}{
		{"relative dir", []string{"--dir", "relative-root"}},
		{"extra argument", []string{"--dir", dir, "extra"}},
		{"unbounded timeout", []string{"--dir", dir, "--timeout", "0s"}},
	}
	for _, tc := range usageCases {
		before := snapshotTree(t, dir)
		var stdout, stderr bytes.Buffer
		if code := runManagedRecover(tc.args, &stdout, &stderr); code != 2 {
			t.Fatalf("%s: exit = %d, want 2 (stderr=%s)", tc.name, code, stderr.String())
		}
		if stdout.Len() != 0 {
			t.Fatalf("%s: stdout = %q, want empty", tc.name, stdout.String())
		}
		if after := snapshotTree(t, dir); !equalSnapshots(before, after) {
			t.Fatalf("%s: usage refusal changed the tree", tc.name)
		}
	}

	var stdout, stderr bytes.Buffer
	code := runManagedRecover([]string{"--dir", dir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr=%s)", code, stderr.String())
	}
	var outcome struct {
		OK             bool   `json:"ok"`
		Present        bool   `json:"present"`
		State          string `json:"state"`
		Entries        int    `json:"entries"`
		Restored       bool   `json:"restored"`
		RemovedJournal bool   `json:"removed_journal"`
		RemovedStaging bool   `json:"removed_staging"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &outcome); err != nil {
		t.Fatalf("decode stdout %q: %v", stdout.String(), err)
	}
	if !outcome.OK || !outcome.Present || outcome.State != "staged" || outcome.Entries != 1 || outcome.Restored {
		t.Fatalf("outcome = %+v, want present staged single-entry not restored", outcome)
	}
	if !outcome.RemovedJournal || !outcome.RemovedStaging {
		t.Fatalf("outcome = %+v, want journal and staging removed", outcome)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	assertNoRecoverJournal(t, dir)

	var secondOut, secondErr bytes.Buffer
	if code := runManagedRecover([]string{"--dir", dir}, &secondOut, &secondErr); code != 0 {
		t.Fatalf("second exit = %d, want 0 (stderr=%s)", code, secondErr.String())
	}
	if want := "{\"ok\":true,\"present\":false}\n"; secondOut.String() != want {
		t.Fatalf("second stdout = %q, want %q", secondOut.String(), want)
	}
}

// TestManagedStateRecoverRefusesLiveOwner covers a retained journal while the
// owner is still held: the command reports contention and leaves the journal.
func TestManagedStateRecoverRefusesLiveOwner(t *testing.T) {
	dir := managedStateRoot(t)
	root, err := managedstate.OpenExistingRoot(dir)
	if err != nil {
		t.Fatalf("OpenExistingRoot: %v", err)
	}
	owner, err := root.TryAcquireOwner()
	if err != nil {
		t.Fatalf("TryAcquireOwner: %v", err)
	}
	if err := owner.PublishRecord(); err != nil {
		t.Fatalf("PublishRecord: %v", err)
	}
	t.Cleanup(func() { _ = owner.Close() })

	txn, err := owner.AcquireTransaction(context.Background(), 0)
	if err != nil {
		t.Fatalf("AcquireTransaction: %v", err)
	}
	if _, err := txn.BeginJournal("run-live", "run-live", []managedstate.JournalEntry{
		{Path: "phone-app-origin-configured", PriorExists: false, PriorMode: 0o600, NewBytes: []byte("https://new.example"), NewMode: 0o600},
	}); err != nil {
		t.Fatalf("BeginJournal: %v", err)
	}
	if err := txn.Release(); err != nil {
		t.Fatalf("Txn.Release: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := runManagedRecover([]string{"--dir", dir}, &stdout, &stderr)
	if code != 3 {
		t.Fatalf("exit = %d, want 3 (stderr=%s)", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), "stop the relay") {
		t.Fatalf("stderr = %q, want relay guidance", stderr.String())
	}
	if !recoverJournalPresent(t, dir) {
		t.Fatal("journal.json was removed while the owner was live")
	}
}

// equalSnapshots compares two snapshotTree results without importing reflect in
// this file.
func equalSnapshots(before, after map[string]string) bool {
	if len(before) != len(after) {
		return false
	}
	for key, value := range before {
		if after[key] != value {
			return false
		}
	}
	return true
}
