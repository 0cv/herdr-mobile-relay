package managedstate_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/managedstate"
)

// retireRecoveryOwner retires the owner cleanly after T has been released, so
// the retained journal is left behind with no owner lock.
func retireRecoveryOwner(t *testing.T, owner *managedstate.Owner) {
	t.Helper()
	noErr(t, owner.BeginClosing(), "BeginClosing")
	noErr(t, owner.Retire(), "Retire")
	noErr(t, owner.Close(), "Owner.Close")
}

// rewriteRecoveryJournal rewrites the on-disk journal state/applied markers by
// hand, standing in for a transaction that crashed after renaming its stage
// files but before writing the committed marker.
func rewriteRecoveryJournal(t *testing.T, dir, state string, applied bool) {
	t.Helper()
	path := filepath.Join(dir, "journal.json")
	raw := readFile(t, path)
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode journal for rewrite: %v", err)
	}
	doc["state"] = state
	entries, ok := doc["entries"].([]any)
	if !ok {
		t.Fatalf("journal entries have type %T", doc["entries"])
	}
	for _, rawEntry := range entries {
		entry, ok := rawEntry.(map[string]any)
		if !ok {
			t.Fatalf("journal entry has type %T", rawEntry)
		}
		entry["applied"] = applied
	}
	encoded, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("encode journal for rewrite: %v", err)
	}
	writeFile(t, path, encoded, 0o600)
}

func applyRecoveryStagesByHand(t *testing.T, dir, txnID string, targets ...string) {
	t.Helper()
	for i, target := range targets {
		stage := filepath.Join(dir, fmt.Sprintf("journal.stage.%s.%d", txnID, i))
		noErr(t, os.Rename(stage, filepath.Join(dir, target)), "hand apply stage")
	}
}

// recoveryTreeSnapshot records relative paths, modes and file contents so the
// absent-journal no-op can be proven to have touched nothing.
func recoveryTreeSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	snapshot := map[string]string{}
	if err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		value := info.Mode().String()
		if info.Mode().IsRegular() {
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			value += ":" + string(data)
		}
		snapshot[rel] = value
		return nil
	}); err != nil {
		t.Fatalf("snapshot %s: %v", dir, err)
	}
	return snapshot
}

// TestRecoverStagedJournalRemovesWithoutMutation covers a stopped owner with a
// clean staged journal: every target is still at its prior state, so recovery
// removes the journal and stage files without touching any target.
func TestRecoverStagedJournalRemovesWithoutMutation(t *testing.T) {
	dir, _, owner, txn := journalSetup(t)

	existing := filepath.Join(dir, "existing.env")
	writeFile(t, existing, []byte("prior-existing"), 0o644)

	journal, err := txn.BeginJournal("txn-recover-staged", "run-recover-staged", []managedstate.JournalEntry{
		{Path: "existing.env", PriorExists: true, PriorBytes: []byte("prior-existing"), PriorMode: 0o644, NewBytes: []byte("next-existing"), NewMode: 0o600},
		{Path: "created.env", PriorExists: false, PriorMode: 0o600, NewBytes: []byte("next-created"), NewMode: 0o644},
	})
	noErr(t, err, "BeginJournal")
	if journal == nil {
		t.Fatal("BeginJournal returned a nil journal")
	}
	noErr(t, txn.Release(), "Txn.Release")
	retireRecoveryOwner(t, owner)

	reopen := openRoot(t, dir)
	defer reopen.Close()
	outcome, err := managedstate.RecoverStagedJournal(context.Background(), reopen)
	noErr(t, err, "RecoverStagedJournal")
	if !outcome.Present || outcome.State != "staged" || outcome.Entries != 2 || outcome.Restored {
		t.Fatalf("outcome = %+v, want present staged two-entry not restored", outcome)
	}
	if !outcome.RemovedJournal || !outcome.RemovedStaging {
		t.Fatalf("outcome = %+v, want journal and staging removed", outcome)
	}
	if got := readFile(t, existing); string(got) != "prior-existing" {
		t.Fatalf("existing.env = %q, want prior bytes", got)
	}
	assertMode(t, existing, 0o644)
	if _, err := os.Lstat(filepath.Join(dir, "created.env")); !os.IsNotExist(err) {
		t.Fatalf("created.env present after staged recovery: %v", err)
	}
	assertNoJournal(t, dir)
}

// TestRecoverAppliedJournalRestoresPrior covers a transaction whose stage
// files were renamed but whose committed marker was never written: recovery
// restores exactly the recorded prior bytes/mode/absence and then removes the
// journal. The record is exercised both as a still-staged record left by a
// hand apply and as an explicitly applied record.
func TestRecoverAppliedJournalRestoresPrior(t *testing.T) {
	cases := []struct {
		name       string
		state      string
		setApplied bool
	}{
		{"staged-record-hand-applied", "", false},
		{"applied-record", "applied", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, _, owner, txn := journalSetup(t)

			existing := filepath.Join(dir, "existing.env")
			writeFile(t, existing, []byte("prior-existing"), 0o644)

			_, err := txn.BeginJournal("txn-recover-applied", "run-recover-applied", []managedstate.JournalEntry{
				{Path: "existing.env", PriorExists: true, PriorBytes: []byte("prior-existing"), PriorMode: 0o644, NewBytes: []byte("next-existing"), NewMode: 0o600},
				{Path: "created.env", PriorExists: false, PriorMode: 0o600, NewBytes: []byte("next-created"), NewMode: 0o644},
			})
			noErr(t, err, "BeginJournal")

			applyRecoveryStagesByHand(t, dir, "txn-recover-applied", "existing.env", "created.env")
			if tc.state != "" {
				rewriteRecoveryJournal(t, dir, tc.state, tc.setApplied)
			}

			noErr(t, txn.Release(), "Txn.Release")
			retireRecoveryOwner(t, owner)

			reopen := openRoot(t, dir)
			defer reopen.Close()
			outcome, err := managedstate.RecoverStagedJournal(context.Background(), reopen)
			noErr(t, err, "RecoverStagedJournal")
			if !outcome.Present || !outcome.Restored {
				t.Fatalf("outcome = %+v, want present restored", outcome)
			}
			if !outcome.RemovedJournal || !outcome.RemovedStaging {
				t.Fatalf("outcome = %+v, want journal and staging removed", outcome)
			}
			if got := readFile(t, existing); string(got) != "prior-existing" {
				t.Fatalf("existing.env = %q, want restored prior bytes", got)
			}
			assertMode(t, existing, 0o644)
			if _, err := os.Lstat(filepath.Join(dir, "created.env")); !os.IsNotExist(err) {
				t.Fatalf("created.env present after applied recovery: %v", err)
			}
			assertNoJournal(t, dir)
		})
	}
}

// TestRecoverCommittedJournalFinishesRemoval covers a committed journal that
// crashed before removing its evidence: the applied targets are kept and the
// journal and stage files are removed without rewriting the targets.
func TestRecoverCommittedJournalFinishesRemoval(t *testing.T) {
	dir, _, owner, txn := journalSetup(t)

	existing := filepath.Join(dir, "existing.env")
	writeFile(t, existing, []byte("prior-existing"), 0o644)

	_, err := txn.BeginJournal("txn-recover-committed", "run-recover-committed", []managedstate.JournalEntry{
		{Path: "existing.env", PriorExists: true, PriorBytes: []byte("prior-existing"), PriorMode: 0o644, NewBytes: []byte("next-existing"), NewMode: 0o600},
		{Path: "created.env", PriorExists: false, PriorMode: 0o600, NewBytes: []byte("next-created"), NewMode: 0o644},
	})
	noErr(t, err, "BeginJournal")

	applyRecoveryStagesByHand(t, dir, "txn-recover-committed", "existing.env", "created.env")
	rewriteRecoveryJournal(t, dir, "committed", true)

	noErr(t, txn.Release(), "Txn.Release")
	retireRecoveryOwner(t, owner)

	reopen := openRoot(t, dir)
	defer reopen.Close()
	outcome, err := managedstate.RecoverStagedJournal(context.Background(), reopen)
	noErr(t, err, "RecoverStagedJournal")
	if !outcome.Present || outcome.State != "committed" || outcome.Restored {
		t.Fatalf("outcome = %+v, want present committed not restored", outcome)
	}
	if !outcome.RemovedJournal || !outcome.RemovedStaging {
		t.Fatalf("outcome = %+v, want journal and staging removed", outcome)
	}
	if got := readFile(t, existing); string(got) != "next-existing" {
		t.Fatalf("existing.env = %q, want committed new bytes", got)
	}
	assertMode(t, existing, 0o600)
	if got := readFile(t, filepath.Join(dir, "created.env")); string(got) != "next-created" {
		t.Fatalf("created.env = %q, want committed new bytes", got)
	}
	assertNoJournal(t, dir)
}

// TestRecoverRefusesOnMismatch covers a foreign edit to a target while a
// journal is staged: recovery refuses with ErrForeignState, retains every
// piece of evidence and changes no target.
func TestRecoverRefusesOnMismatch(t *testing.T) {
	dir, _, owner, txn := journalSetup(t)

	existing := filepath.Join(dir, "existing.env")
	writeFile(t, existing, []byte("prior-existing"), 0o644)

	_, err := txn.BeginJournal("txn-recover-mismatch", "run-recover-mismatch", []managedstate.JournalEntry{
		{Path: "existing.env", PriorExists: true, PriorBytes: []byte("prior-existing"), PriorMode: 0o644, NewBytes: []byte("next-existing"), NewMode: 0o600},
		{Path: "created.env", PriorExists: false, PriorMode: 0o600, NewBytes: []byte("next-created"), NewMode: 0o644},
	})
	noErr(t, err, "BeginJournal")

	writeFile(t, existing, []byte("foreign-edit"), 0o600)
	noErr(t, txn.Release(), "Txn.Release")
	retireRecoveryOwner(t, owner)

	reopen := openRoot(t, dir)
	defer reopen.Close()
	if _, err := managedstate.RecoverStagedJournal(context.Background(), reopen); !errors.Is(err, managedstate.ErrForeignState) {
		t.Fatalf("RecoverStagedJournal: got %v, want %v", err, managedstate.ErrForeignState)
	}
	assertJournalPresent(t, dir)
	if got := readFile(t, existing); string(got) != "foreign-edit" {
		t.Fatalf("foreign target changed: %q", got)
	}
	if _, err := os.Lstat(filepath.Join(dir, "created.env")); !os.IsNotExist(err) {
		t.Fatalf("created.env appeared during a refused recovery: %v", err)
	}
}

// TestRecoverRefusesWithLiveOwner covers a retained journal while the owner is
// still live: recovery refuses with ErrBusy before acquiring T and retains the
// journal untouched.
func TestRecoverRefusesWithLiveOwner(t *testing.T) {
	dir, root, _, txn := journalSetup(t)

	_, err := txn.BeginJournal("txn-recover-live", "run-recover-live", []managedstate.JournalEntry{
		{Path: "created.env", PriorExists: false, PriorMode: 0o600, NewBytes: []byte("next-created"), NewMode: 0o600},
	})
	noErr(t, err, "BeginJournal")
	noErr(t, txn.Release(), "Txn.Release")

	if _, err := managedstate.RecoverStagedJournal(context.Background(), root); !errors.Is(err, managedstate.ErrBusy) {
		t.Fatalf("RecoverStagedJournal: got %v, want %v", err, managedstate.ErrBusy)
	}
	assertJournalPresent(t, dir)
	if _, err := os.Lstat(filepath.Join(dir, "owner.lock")); err != nil {
		t.Fatalf("owner.lock was touched: %v", err)
	}
}

// TestRecoverAbsentJournalNoop covers a stopped owner with no journal: recovery
// reports Present false and changes nothing.
func TestRecoverAbsentJournalNoop(t *testing.T) {
	dir, _, owner, txn := journalSetup(t)
	noErr(t, txn.Release(), "Txn.Release")
	retireRecoveryOwner(t, owner)

	reopen := openRoot(t, dir)
	defer reopen.Close()
	before := recoveryTreeSnapshot(t, dir)
	outcome, err := managedstate.RecoverStagedJournal(context.Background(), reopen)
	noErr(t, err, "RecoverStagedJournal")
	if outcome.Present {
		t.Fatalf("outcome = %+v, want Present false", outcome)
	}
	after := recoveryTreeSnapshot(t, dir)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("absent-journal recovery changed the tree: before=%v after=%v", before, after)
	}
}
