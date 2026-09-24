package managedstate_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/managedstate"
)

// journalSetup acquires a real B1/S9A root, owner and held transaction.
func journalSetup(t *testing.T) (string, *managedstate.Root, *managedstate.Owner, *managedstate.Txn) {
	t.Helper()
	dir := mkRoot(t)
	root := openRoot(t, dir)
	owner := acquireOwner(t, root)
	publishOwner(t, owner)
	txn := acquireTxn(t, owner, 0)
	t.Cleanup(func() {
		_ = txn.Release()
		_ = owner.Close()
		_ = root.Close()
	})
	return dir, root, owner, txn
}

func journalStagedFiles(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "journal.stage.*"))
	if err != nil {
		t.Fatalf("glob staged files: %v", err)
	}
	return matches
}

func assertNoJournal(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(dir, "journal.json")); !os.IsNotExist(err) {
		t.Fatalf("journal.json still present: %v", err)
	}
	if matches := journalStagedFiles(t, dir); len(matches) != 0 {
		t.Fatalf("staged files still present: %v", matches)
	}
}

func assertJournalPresent(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(dir, "journal.json")); err != nil {
		t.Fatalf("journal.json missing: %v", err)
	}
}

func journalEntryFor(path string) managedstate.JournalEntry {
	return managedstate.JournalEntry{
		Path:        path,
		PriorExists: false,
		PriorMode:   0o600,
		NewBytes:    []byte("data-" + path),
		NewMode:     0o600,
	}
}

// TestJournalApplyWritesAndCommits covers the happy path: exact bytes and modes
// are written, the journal and stage files are removed only after commit, and a
// second Apply is refused.
func TestJournalApplyWritesAndCommits(t *testing.T) {
	dir, _, _, txn := journalSetup(t)

	existing := filepath.Join(dir, "existing.env")
	writeFile(t, existing, []byte("old-bytes"), 0o600)
	created := filepath.Join(dir, "created.env")

	journal, err := txn.BeginJournal("txn-apply", "run-apply", []managedstate.JournalEntry{
		{Path: "existing.env", PriorExists: true, PriorBytes: []byte("old-bytes"), PriorMode: 0o600, NewBytes: []byte("new-existing"), NewMode: 0o600},
		{Path: "created.env", PriorExists: false, PriorMode: 0o600, NewBytes: []byte("new-created"), NewMode: 0o644},
	})
	noErr(t, err, "BeginJournal")
	if journal == nil {
		t.Fatal("BeginJournal returned a nil journal")
	}
	assertJournalPresent(t, dir)
	noErr(t, journal.Apply(), "Journal.Apply")

	if got := readFile(t, existing); string(got) != "new-existing" {
		t.Fatalf("existing.env = %q, want %q", got, "new-existing")
	}
	assertMode(t, existing, 0o600)
	if got := readFile(t, created); string(got) != "new-created" {
		t.Fatalf("created.env = %q, want %q", got, "new-created")
	}
	assertMode(t, created, 0o644)
	assertNoJournal(t, dir)

	if err := journal.Apply(); !errors.Is(err, managedstate.ErrUnknownAuthority) {
		t.Fatalf("second Apply: got %v, want %v", err, managedstate.ErrUnknownAuthority)
	}
}

// TestJournalRollbackRestoresPriorBytesAndAbsence covers rollback after staging
// alone and after a hand-applied partial apply.
func TestJournalRollbackRestoresPriorBytesAndAbsence(t *testing.T) {
	t.Run("staged-only", func(t *testing.T) {
		dir, _, _, txn := journalSetup(t)
		existing := filepath.Join(dir, "existing.env")
		writeFile(t, existing, []byte("old-bytes"), 0o600)

		journal, err := txn.BeginJournal("txn-rb1", "run-rb1", []managedstate.JournalEntry{
			{Path: "existing.env", PriorExists: true, PriorBytes: []byte("old-bytes"), PriorMode: 0o600, NewBytes: []byte("replacement"), NewMode: 0o600},
			{Path: "created.env", PriorExists: false, PriorMode: 0o600, NewBytes: []byte("brand-new"), NewMode: 0o644},
		})
		noErr(t, err, "BeginJournal")
		noErr(t, journal.Rollback(), "Rollback")

		if got := readFile(t, existing); string(got) != "old-bytes" {
			t.Fatalf("existing.env = %q, want %q", got, "old-bytes")
		}
		assertMode(t, existing, 0o600)
		if _, err := os.Lstat(filepath.Join(dir, "created.env")); !os.IsNotExist(err) {
			t.Fatalf("created.env still present: %v", err)
		}
		assertNoJournal(t, dir)
	})

	t.Run("partial-apply", func(t *testing.T) {
		dir, _, _, txn := journalSetup(t)
		existing := filepath.Join(dir, "existing.env")
		writeFile(t, existing, []byte("old-bytes"), 0o600)

		journal, err := txn.BeginJournal("txn-rb2", "run-rb2", []managedstate.JournalEntry{
			{Path: "existing.env", PriorExists: true, PriorBytes: []byte("old-bytes"), PriorMode: 0o600, NewBytes: []byte("replacement"), NewMode: 0o600},
			{Path: "created.env", PriorExists: false, PriorMode: 0o600, NewBytes: []byte("brand-new"), NewMode: 0o644},
		})
		noErr(t, err, "BeginJournal")

		// Apply exactly one entry by hand through its mandated stage file.
		stage0 := filepath.Join(dir, "journal.stage.txn-rb2.0")
		noErr(t, os.Rename(stage0, existing), "hand apply entry 0")
		if got := readFile(t, existing); string(got) != "replacement" {
			t.Fatalf("hand-applied existing.env = %q, want %q", got, "replacement")
		}

		noErr(t, journal.Rollback(), "Rollback")
		if got := readFile(t, existing); string(got) != "old-bytes" {
			t.Fatalf("existing.env = %q, want %q", got, "old-bytes")
		}
		assertMode(t, existing, 0o600)
		if _, err := os.Lstat(filepath.Join(dir, "created.env")); !os.IsNotExist(err) {
			t.Fatalf("created.env still present: %v", err)
		}
		assertNoJournal(t, dir)
	})
}

// TestJournalRollbackRefusesOnMismatch proves a foreign edit stops rollback,
// returns ForeignState and retains the journal as evidence.
func TestJournalRollbackRefusesOnMismatch(t *testing.T) {
	dir, _, _, txn := journalSetup(t)
	existing := filepath.Join(dir, "existing.env")
	writeFile(t, existing, []byte("old-bytes"), 0o600)

	journal, err := txn.BeginJournal("txn-rb3", "run-rb3", []managedstate.JournalEntry{
		{Path: "existing.env", PriorExists: true, PriorBytes: []byte("old-bytes"), PriorMode: 0o600, NewBytes: []byte("replacement"), NewMode: 0o600},
		{Path: "created.env", PriorExists: false, PriorMode: 0o600, NewBytes: []byte("brand-new"), NewMode: 0o600},
	})
	noErr(t, err, "BeginJournal")

	writeFile(t, existing, []byte("foreign-edit"), 0o600)
	if err := journal.Rollback(); !errors.Is(err, managedstate.ErrForeignState) {
		t.Fatalf("Rollback: got %v, want %v", err, managedstate.ErrForeignState)
	}
	assertJournalPresent(t, dir)
	if got := readFile(t, existing); string(got) != "foreign-edit" {
		t.Fatalf("foreign bytes changed: %q", got)
	}
}

// TestJournalRejectsUnsafeOrOversizedEntries proves every unsafe or oversized
// entry is refused before any journal or stage file is created.
func TestJournalRejectsUnsafeOrOversizedEntries(t *testing.T) {
	dir, _, _, txn := journalSetup(t)
	small := []byte("new")

	manyEntries := func(count int) []managedstate.JournalEntry {
		entries := make([]managedstate.JournalEntry, 0, count)
		for i := 0; i < count; i++ {
			entries = append(entries, journalEntryFor(fmt.Sprintf("file-%d.env", i)))
		}
		return entries
	}

	cases := []struct {
		name    string
		entries []managedstate.JournalEntry
	}{
		{"absolute", []managedstate.JournalEntry{{Path: "/abs.env", PriorMode: 0o600, NewBytes: small, NewMode: 0o600}}},
		{"dotdot", []managedstate.JournalEntry{{Path: "..", PriorMode: 0o600, NewBytes: small, NewMode: 0o600}}},
		{"separator", []managedstate.JournalEntry{{Path: "../escape", PriorMode: 0o600, NewBytes: small, NewMode: 0o600}}},
		{"empty", []managedstate.JournalEntry{{Path: "", PriorMode: 0o600, NewBytes: small, NewMode: 0o600}}},
		{"duplicate", []managedstate.JournalEntry{journalEntryFor("dup.env"), journalEntryFor("dup.env")}},
		{"reserved-journal", []managedstate.JournalEntry{{Path: "journal.json", PriorMode: 0o600, NewBytes: small, NewMode: 0o600}}},
		{"reserved-stage", []managedstate.JournalEntry{{Path: "journal.stage.txn-bad.0", PriorMode: 0o600, NewBytes: small, NewMode: 0o600}}},
		{"reserved-owner-lock", []managedstate.JournalEntry{{Path: "owner.lock", PriorMode: 0o600, NewBytes: small, NewMode: 0o600}}},
		{"reserved-txn-lock", []managedstate.JournalEntry{{Path: "txn.lock", PriorMode: 0o600, NewBytes: small, NewMode: 0o600}}},
		{"reserved-generation", []managedstate.JournalEntry{{Path: "generation", PriorMode: 0o600, NewBytes: small, NewMode: 0o600}}},
		{"too-many", manyEntries(33)},
		{"encoded-overflow", []managedstate.JournalEntry{
			{Path: "e1.env", PriorExists: true, PriorBytes: make([]byte, 6144), PriorMode: 0o600, NewBytes: small, NewMode: 0o600},
			{Path: "e2.env", PriorExists: true, PriorBytes: make([]byte, 6144), PriorMode: 0o600, NewBytes: small, NewMode: 0o600},
		}},
		{"prior-byte-limit", []managedstate.JournalEntry{{Path: "big-prior.env", PriorExists: true, PriorBytes: make([]byte, 8*1024+1), PriorMode: 0o600, NewBytes: small, NewMode: 0o600}}},
		{"prior-total-limit", []managedstate.JournalEntry{
			{Path: "t1.env", PriorExists: true, PriorBytes: make([]byte, 7*1024), PriorMode: 0o600, NewBytes: small, NewMode: 0o600},
			{Path: "t2.env", PriorExists: true, PriorBytes: make([]byte, 7*1024), PriorMode: 0o600, NewBytes: small, NewMode: 0o600},
		}},
		{"new-byte-limit", []managedstate.JournalEntry{{Path: "big-new.env", PriorExists: false, PriorMode: 0o600, NewBytes: make([]byte, 1<<20+1), NewMode: 0o600}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := txn.BeginJournal("txn-bad", "run-bad", tc.entries); !errors.Is(err, managedstate.ErrInvalidRecord) {
				t.Fatalf("BeginJournal: got %v, want %v", err, managedstate.ErrInvalidRecord)
			}
			if _, err := os.Lstat(filepath.Join(dir, "journal.json")); !os.IsNotExist(err) {
				t.Fatalf("journal.json created: %v", err)
			}
			if matches := journalStagedFiles(t, dir); len(matches) != 0 {
				t.Fatalf("stage files created: %v", matches)
			}
		})
	}
}

// TestJournalEncodedRecordStaysWithinDecodeBound proves a journal that fits the
// strict 16 KiB record bound is staged and is readable by RecoverJournal, and
// that the pre-write encoded-size check keeps raw-legal entries from producing
// an unreadable record. The rejected two-entry 12288-byte case is covered in
// TestJournalRejectsUnsafeOrOversizedEntries/encoded-overflow.
func TestJournalEncodedRecordStaysWithinDecodeBound(t *testing.T) {
	dir, root, _, txn := journalSetup(t)
	prior := bytes.Repeat([]byte("p"), 6000)

	journal, err := txn.BeginJournal("txn-bound", "run-bound", []managedstate.JournalEntry{
		{Path: "near-bound.env", PriorExists: true, PriorBytes: prior, PriorMode: 0o600, NewBytes: []byte("new"), NewMode: 0o600},
	})
	noErr(t, err, "BeginJournal near the encoded bound")
	if journal == nil {
		t.Fatal("BeginJournal returned a nil journal")
	}

	info, err := os.Lstat(filepath.Join(dir, "journal.json"))
	if err != nil {
		t.Fatalf("lstat journal.json: %v", err)
	}
	if info.Size() > 16*1024 {
		t.Fatalf("journal.json size = %d, want <= %d", info.Size(), 16*1024)
	}

	noErr(t, txn.Release(), "Txn.Release")
	status, err := managedstate.RecoverJournal(root)
	noErr(t, err, "RecoverJournal")
	if !status.Present || status.State != "staged" || status.TxnID != "txn-bound" || status.Entries != 1 {
		t.Fatalf("RecoverJournal = %+v, want present staged txn-bound with 1 entry", status)
	}
}

// TestJournalLifecycleEncodingBound covers F003: BeginJournal must bound the
// maximum lifecycle encoding (including the longer "committed" state), not
// only the initial "staged" shape. The two cases below sit exactly on the
// 16 KiB boundary with filesystem-valid names (path lengths <= 255): the
// committed shape is one byte larger than the staged shape for two entries.
// The overflowing case must be rejected at BeginJournal with no evidence left;
// the fitting case must apply to completion and clean up.
func TestJournalLifecycleEncodingBound(t *testing.T) {
	t.Run("committed-overflow-rejected", func(t *testing.T) {
		dir, _, _, txn := journalSetup(t)
		journal, err := txn.BeginJournal("t", "r", []managedstate.JournalEntry{
			{Path: strings.Repeat("a", 255), PriorExists: true, PriorBytes: make([]byte, 4000), PriorMode: 0o600, NewBytes: []byte("x"), NewMode: 0o600},
			{Path: strings.Repeat("b", 254), PriorExists: true, PriorBytes: make([]byte, 7543), PriorMode: 0o600, NewBytes: []byte("x"), NewMode: 0o600},
		})
		if !errors.Is(err, managedstate.ErrInvalidRecord) {
			t.Fatalf("BeginJournal: got %v, want %v", err, managedstate.ErrInvalidRecord)
		}
		if journal != nil {
			t.Fatal("BeginJournal returned a journal for an oversized committed record")
		}
		if _, err := os.Lstat(filepath.Join(dir, "journal.json")); !os.IsNotExist(err) {
			t.Fatalf("journal.json created: %v", err)
		}
		if matches := journalStagedFiles(t, dir); len(matches) != 0 {
			t.Fatalf("stage files created: %v", matches)
		}
	})

	t.Run("at-bound-applies", func(t *testing.T) {
		dir, _, _, txn := journalSetup(t)
		journal, err := txn.BeginJournal("t", "r", []managedstate.JournalEntry{
			{Path: strings.Repeat("a", 254), PriorExists: true, PriorBytes: make([]byte, 4000), PriorMode: 0o600, NewBytes: []byte("x"), NewMode: 0o600},
			{Path: strings.Repeat("b", 254), PriorExists: true, PriorBytes: make([]byte, 7543), PriorMode: 0o600, NewBytes: []byte("x"), NewMode: 0o600},
		})
		noErr(t, err, "BeginJournal at the lifecycle bound")
		noErr(t, journal.Apply(), "Apply at the lifecycle bound")
		if _, err := os.Lstat(filepath.Join(dir, "journal.json")); !os.IsNotExist(err) {
			t.Fatalf("journal.json still present after Apply: %v", err)
		}
	})
}

// TestJournalStagedCrashIsRecoverable proves a released transaction leaves a
// readable staged journal and a corrupt journal is reported without mutation.
func TestJournalStagedCrashIsRecoverable(t *testing.T) {
	dir, root, _, txn := journalSetup(t)

	_, err := txn.BeginJournal("txn-crash", "run-crash", []managedstate.JournalEntry{journalEntryFor("one.env")})
	noErr(t, err, "BeginJournal")
	noErr(t, txn.Release(), "Txn.Release")

	status, err := managedstate.RecoverJournal(root)
	noErr(t, err, "RecoverJournal")
	if !status.Present {
		t.Fatal("RecoverJournal Present = false, want true")
	}
	if status.TxnID != "txn-crash" {
		t.Fatalf("TxnID = %q, want %q", status.TxnID, "txn-crash")
	}
	if status.RunID != "run-crash" {
		t.Fatalf("RunID = %q, want %q", status.RunID, "run-crash")
	}
	if status.State != "staged" {
		t.Fatalf("State = %q, want %q", status.State, "staged")
	}
	if status.Entries != 1 {
		t.Fatalf("Entries = %d, want 1", status.Entries)
	}

	journalPath := filepath.Join(dir, "journal.json")
	corrupt := []byte("{not-valid-json")
	writeFile(t, journalPath, corrupt, 0o600)
	if _, err := managedstate.RecoverJournal(root); !errors.Is(err, managedstate.ErrInvalidRecord) {
		t.Fatalf("corrupt RecoverJournal: got %v, want %v", err, managedstate.ErrInvalidRecord)
	}
	if got := readFile(t, journalPath); !bytes.Equal(got, corrupt) {
		t.Fatalf("corrupt journal changed: %q", got)
	}
}

// TestJournalApplyFailureKeepsRollbackViable proves an apply that fails after
// staging keeps a durable journal and that rollback still restores every entry.
func TestJournalApplyFailureKeepsRollbackViable(t *testing.T) {
	t.Run("unwritable-root", func(t *testing.T) {
		dir, _, _, txn := journalSetup(t)
		first := filepath.Join(dir, "first.env")
		writeFile(t, first, []byte("prior-first"), 0o600)
		second := filepath.Join(dir, "second.env")
		writeFile(t, second, []byte("prior-second"), 0o600)

		journal, err := txn.BeginJournal("txn-ro", "run-ro", []managedstate.JournalEntry{
			{Path: "first.env", PriorExists: true, PriorBytes: []byte("prior-first"), PriorMode: 0o600, NewBytes: []byte("next-first"), NewMode: 0o600},
			{Path: "second.env", PriorExists: true, PriorBytes: []byte("prior-second"), PriorMode: 0o600, NewBytes: []byte("next-second"), NewMode: 0o600},
		})
		noErr(t, err, "BeginJournal")

		noErr(t, os.Chmod(dir, 0o500), "make root unwritable")
		applyErr := journal.Apply()
		noErr(t, os.Chmod(dir, 0o700), "restore root writable")
		if applyErr == nil {
			t.Fatal("Apply under an unwritable root unexpectedly succeeded")
		}
		assertJournalPresent(t, dir)

		noErr(t, journal.Rollback(), "Rollback after failure")
		if got := readFile(t, first); string(got) != "prior-first" {
			t.Fatalf("first.env = %q, want %q", got, "prior-first")
		}
		if got := readFile(t, second); string(got) != "prior-second" {
			t.Fatalf("second.env = %q, want %q", got, "prior-second")
		}
		assertNoJournal(t, dir)
	})

	t.Run("partial-apply", func(t *testing.T) {
		dir, _, _, txn := journalSetup(t)
		first := filepath.Join(dir, "first.env")
		writeFile(t, first, []byte("prior-first"), 0o600)
		second := filepath.Join(dir, "second.env")
		writeFile(t, second, []byte("prior-second"), 0o600)

		journal, err := txn.BeginJournal("txn-pa", "run-pa", []managedstate.JournalEntry{
			{Path: "first.env", PriorExists: true, PriorBytes: []byte("prior-first"), PriorMode: 0o600, NewBytes: []byte("next-first"), NewMode: 0o600},
			{Path: "second.env", PriorExists: true, PriorBytes: []byte("prior-second"), PriorMode: 0o600, NewBytes: []byte("next-second"), NewMode: 0o600},
		})
		noErr(t, err, "BeginJournal")

		noErr(t, os.Remove(filepath.Join(dir, "journal.stage.txn-pa.1")), "drop staged entry 1")
		if err := journal.Apply(); err == nil {
			t.Fatal("Apply with a missing stage file unexpectedly succeeded")
		}
		assertJournalPresent(t, dir)
		if got := readFile(t, first); string(got) != "next-first" {
			t.Fatalf("first.env = %q, want the applied %q", got, "next-first")
		}

		noErr(t, journal.Rollback(), "Rollback after partial apply")
		if got := readFile(t, first); string(got) != "prior-first" {
			t.Fatalf("first.env = %q, want %q", got, "prior-first")
		}
		if got := readFile(t, second); string(got) != "prior-second" {
			t.Fatalf("second.env = %q, want %q", got, "prior-second")
		}
		assertNoJournal(t, dir)
	})
}

// TestJournalDecodeIsStrictAndBounded feeds malformed journals straight to
// RecoverJournal and proves each is rejected without rewriting the file.
func TestJournalDecodeIsStrictAndBounded(t *testing.T) {
	dir, root, _, _ := journalSetup(t)
	journalPath := filepath.Join(dir, "journal.json")

	valid := fmt.Sprintf(`{"schema":1,"txn_id":"txn-decode","run_id":"run-decode","state":"staged","entries":[{"path":"a.env","prior_exists":false,"prior_mode":384,"new_mode":384,"new_sha256":%q,"staged":"journal.stage.txn-decode.0","applied":false}]}`, strings.Repeat("a", 64))
	writeFile(t, journalPath, []byte(valid), 0o600)
	if _, err := managedstate.RecoverJournal(root); err != nil {
		t.Fatalf("valid journal rejected: %v", err)
	}

	variants := []struct {
		name string
		data string
	}{
		{"duplicate-key", strings.Replace(valid, `"schema":1`, `"schema":1,"schema":1`, 1)},
		{"unknown-member", valid[:len(valid)-1] + `,"extra":1}`},
		{"wrong-type", strings.Replace(valid, `"state":"staged"`, `"state":5`, 1)},
		{"entries-wrong-type", `{"schema":1,"txn_id":"txn-decode","run_id":"run-decode","state":"staged","entries":5}`},
		{"oversized", strings.Replace(valid, `"run_id":"run-decode"`, `"run_id":"`+strings.Repeat("x", 20*1024)+`"`, 1)},
		{"trailing-data", valid + " trailing"},
	}
	for _, tc := range variants {
		t.Run(tc.name, func(t *testing.T) {
			writeFile(t, journalPath, []byte(tc.data), 0o600)
			if _, err := managedstate.RecoverJournal(root); !errors.Is(err, managedstate.ErrInvalidRecord) {
				t.Fatalf("RecoverJournal: got %v, want %v", err, managedstate.ErrInvalidRecord)
			}
			if got := readFile(t, journalPath); string(got) != tc.data {
				t.Fatalf("journal was rewritten: %q", got)
			}
		})
	}
}

// journalRootSetup acquires a real root and a T-only transaction with no owner.
func journalRootSetup(t *testing.T) (string, *managedstate.Root, *managedstate.Txn) {
	t.Helper()
	dir := mkRoot(t)
	root := openRoot(t, dir)
	txn, err := root.AcquireTransaction(context.Background(), 0)
	noErr(t, err, "Root.AcquireTransaction")
	t.Cleanup(func() {
		_ = txn.Release()
		_ = root.Close()
	})
	return dir, root, txn
}

// TestJournalRequiresHeldTransaction proves a released Txn and a closed owner
// both refuse BeginJournal.
func TestJournalRequiresHeldTransaction(t *testing.T) {
	t.Run("released", func(t *testing.T) {
		_, _, _, txn := journalSetup(t)
		noErr(t, txn.Release(), "Txn.Release")
		if _, err := txn.BeginJournal("txn-x", "run-x", []managedstate.JournalEntry{journalEntryFor("a.env")}); !errors.Is(err, managedstate.ErrUnknownAuthority) {
			t.Fatalf("BeginJournal: got %v, want %v", err, managedstate.ErrUnknownAuthority)
		}
	})

	t.Run("closed-owner", func(t *testing.T) {
		_, _, owner, txn := journalSetup(t)
		noErr(t, owner.Close(), "Owner.Close")
		if _, err := txn.BeginJournal("txn-x", "run-x", []managedstate.JournalEntry{journalEntryFor("a.env")}); !errors.Is(err, managedstate.ErrUnknownAuthority) {
			t.Fatalf("BeginJournal: got %v, want %v", err, managedstate.ErrUnknownAuthority)
		}
	})
}

// TestJournalRootTransactionJournaling proves a T-only transaction can stage
// and apply a file, then is refused on a second Apply.
func TestJournalRootTransactionJournaling(t *testing.T) {
	dir, _, txn := journalRootSetup(t)

	journal, err := txn.BeginJournal("txn-t-only-apply", "run-t-only-apply", []managedstate.JournalEntry{
		{Path: "created.env", PriorExists: false, PriorMode: 0o600, NewBytes: []byte("t-only-created"), NewMode: 0o644},
	})
	noErr(t, err, "BeginJournal")
	if journal == nil {
		t.Fatal("BeginJournal returned a nil journal")
	}
	assertJournalPresent(t, dir)
	noErr(t, journal.Apply(), "Journal.Apply")

	target := filepath.Join(dir, "created.env")
	if got := readFile(t, target); string(got) != "t-only-created" {
		t.Fatalf("created.env = %q, want %q", got, "t-only-created")
	}
	assertMode(t, target, 0o644)
	assertNoJournal(t, dir)

	if err := journal.Apply(); !errors.Is(err, managedstate.ErrUnknownAuthority) {
		t.Fatalf("second Apply: got %v, want %v", err, managedstate.ErrUnknownAuthority)
	}
	noErr(t, txn.Release(), "Txn.Release")
}

// TestJournalRootTransactionRollback proves a T-only transaction restores a
// prior file and removes a created one on Rollback.
func TestJournalRootTransactionRollback(t *testing.T) {
	dir, _, txn := journalRootSetup(t)

	existing := filepath.Join(dir, "existing.env")
	writeFile(t, existing, []byte("prior-t-only"), 0o600)

	journal, err := txn.BeginJournal("txn-t-only-rollback", "run-t-only-rollback", []managedstate.JournalEntry{
		{Path: "existing.env", PriorExists: true, PriorBytes: []byte("prior-t-only"), PriorMode: 0o600, NewBytes: []byte("replacement-t-only"), NewMode: 0o644},
		{Path: "created.env", PriorExists: false, PriorMode: 0o600, NewBytes: []byte("created-t-only"), NewMode: 0o644},
	})
	noErr(t, err, "BeginJournal")
	noErr(t, journal.Rollback(), "Journal.Rollback")

	if got := readFile(t, existing); string(got) != "prior-t-only" {
		t.Fatalf("existing.env = %q, want %q", got, "prior-t-only")
	}
	assertMode(t, existing, 0o600)
	if _, err := os.Lstat(filepath.Join(dir, "created.env")); !os.IsNotExist(err) {
		t.Fatalf("created.env still present: %v", err)
	}
	assertNoJournal(t, dir)
}
