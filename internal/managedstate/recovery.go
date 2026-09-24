package managedstate

// S7C stopped-owner recovery for retained journals.
//
// RecoverStagedJournal reconciles exactly one durable S7A journal after the
// operator has stopped the owner. It never adopts, retires or otherwise
// touches the owner lock, and it never takes over a live or crashed owner: if
// owner.lock exists the call refuses with ErrBusy before reading or writing
// anything. Once T is held it verifies every recorded target before mutating
// any of them, restores only the prior bytes/absence/mode the transaction
// recorded, finishes a committed removal without rewriting targets, and
// retains the journal and stage files as evidence on any mismatch or invalid
// record.
//
// Recovery never touches owner.lock, generation, the device store or any
// invitation, and it makes no claim about whether an invitation persisted.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// recoveryAcquireLimit bounds transaction acquisition inside the recovery API.
// The caller's context further bounds the wait.
const recoveryAcquireLimit = 5 * time.Second

// RecoveryOutcome describes what a stopped-owner recovery observed and did.
type RecoveryOutcome struct {
	Present        bool
	State          string
	Entries        int
	Restored       bool
	RemovedJournal bool
	RemovedStaging bool
}

// RecoverStagedJournal reconciles one retained journal for a stopped owner.
//
// It refuses with ErrBusy while any owner.lock exists, acquires T under the
// bounded transaction acquisition, releases it on every path, and returns
// ErrInvalidRecord for an unreadable journal without rewriting it.
func RecoverStagedJournal(ctx context.Context, root *Root) (RecoveryOutcome, error) {
	if root == nil || root.closed {
		return RecoveryOutcome{}, ErrUnknownAuthority
	}
	// A live or crashed owner is never taken over: its lock is left exactly as
	// found and recovery refuses before acquiring T or reading the journal.
	switch _, err := os.Lstat(filepath.Join(root.path, ownerLockName)); {
	case err == nil:
		return RecoveryOutcome{}, ErrBusy
	case !os.IsNotExist(err):
		return RecoveryOutcome{}, ErrIOFailure
	}
	if ctx == nil {
		ctx = context.Background()
	}
	txn, err := root.AcquireTransaction(ctx, recoveryAcquireLimit)
	if err != nil {
		if errors.Is(err, ErrBusy) || errors.Is(err, ErrTimeout) {
			return RecoveryOutcome{}, ErrBusy
		}
		return RecoveryOutcome{}, err
	}
	defer txn.Release()

	journal, present, err := readRetainedJournal(root)
	if err != nil {
		return RecoveryOutcome{}, err
	}
	if !present {
		return RecoveryOutcome{Present: false}, nil
	}

	// Phase one: classify every target before mutating any of them.
	kinds := make([]journalTargetKind, len(journal.entries))
	for i := range journal.entries {
		kind, err := classifyJournalTarget(root.path, &journal.entries[i])
		if err != nil {
			return RecoveryOutcome{}, err
		}
		if kind == journalTargetForeign {
			return RecoveryOutcome{}, ErrForeignState
		}
		kinds[i] = kind
	}

	restore := false
	switch journal.state {
	case journalStateCommitted:
		for i := range kinds {
			if kinds[i] != journalTargetNew {
				return RecoveryOutcome{}, ErrForeignState
			}
		}
	case journalStateApplied:
		for i := range journal.entries {
			if journal.entries[i].applied {
				if kinds[i] != journalTargetNew {
					return RecoveryOutcome{}, ErrForeignState
				}
				continue
			}
			if kinds[i] != journalTargetPrior {
				return RecoveryOutcome{}, ErrForeignState
			}
		}
		restore = true
	case journalStateStaged:
		// A staged journal whose targets are all still at their prior state is
		// clean evidence to remove. If any target already matches the new
		// value the rename happened (possibly by hand before commit), so the
		// recorded prior state is restored instead.
		for i := range kinds {
			if kinds[i] == journalTargetNew {
				restore = true
				break
			}
		}
	default:
		return RecoveryOutcome{}, ErrInvalidRecord
	}

	// Phase two: every target has been verified, so restore the recorded prior
	// bytes/absence/mode and only then remove the durable evidence.
	if restore {
		for i := range journal.entries {
			if err := restoreJournalPrior(root.path, &journal.entries[i]); err != nil {
				return RecoveryOutcome{}, err
			}
		}
	}
	if err := journal.removeJournalAndStages(); err != nil {
		return RecoveryOutcome{}, err
	}
	return RecoveryOutcome{
		Present:        true,
		State:          journal.state,
		Entries:        len(journal.entries),
		Restored:       restore,
		RemovedJournal: true,
		RemovedStaging: true,
	}, nil
}

// readRetainedJournal reads the strict bounded journal record, if any. A
// malformed or oversized record is rejected with ErrInvalidRecord and left
// untouched on disk.
func readRetainedJournal(root *Root) (*Journal, bool, error) {
	path := filepath.Join(root.path, journalName)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, ErrIOFailure
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || nlink(info) > 1 {
		return nil, false, ErrInvalidRecord
	}
	if info.Size() > maxRecordBytes {
		return nil, false, ErrInvalidRecord
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false, ErrInvalidRecord
	}
	journal, err := decodeJournal(data)
	if err != nil {
		return nil, false, err
	}
	journal.root = root
	return journal, true, nil
}
