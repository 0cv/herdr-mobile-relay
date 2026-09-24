package app

// Managed ownership wiring for a managed foreground serve process.
//
// The long-lived serve process is the single writer of managed state. It takes
// the B1 ownership lock (O) before opening the device store or the control
// socket and retires that lock on normal exit. Acquisition is strictly
// fail-closed: a Busy, Timeout, ChangedRoot, ForeignState, InvalidRecord,
// UnknownAuthority, RetainedEvidence, RandomFailure or IOFailure result is
// reported and nothing is reclaimed, removed or overwritten. A SIGKILL leaves
// the lock in place as retained evidence with no automatic recovery.
//
// This file only wraps the frozen internal/managedstate helper. It deliberately
// defines no package-private identifiers consumed by server.go so the S6B2
// permissive-owner mutant can replace it wholesale.

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/0cv/herdr-mobile-relay/internal/managedstate"
)

// ManagedOwner is an acquired and published managed ownership generation. It
// owns the retained B1 root handle until Close or a successful retirement.
type ManagedOwner struct {
	root      *managedstate.Root
	owner     *managedstate.Owner
	directory string
}

// AcquireManagedOwner opens the canonical managed root, takes the ownership
// lock and publishes the owner record. On any failure the root handle is closed
// and the error names the managed classification. It never retries and never
// removes or reclaims state.
func AcquireManagedOwner(runtimeDir string) (*ManagedOwner, error) {
	root, err := managedstate.OpenExistingRoot(runtimeDir)
	if err != nil {
		return nil, managedAcquireError(err)
	}
	owner, err := root.TryAcquireOwner()
	if err != nil {
		_ = root.Close()
		return nil, managedAcquireError(err)
	}
	if err := owner.PublishRecord(); err != nil {
		_ = root.Close()
		return nil, managedAcquireError(err)
	}
	directory, err := filepath.EvalSymlinks(runtimeDir)
	if err != nil {
		_ = root.Close()
		return nil, managedAcquireError(err)
	}
	return &ManagedOwner{root: root, owner: owner, directory: directory}, nil
}

// RetireManagedOwner releases the ownership lock on normal shutdown. A nil
// owner is a no-op. A refusal returns the error and leaves the lock and record
// untouched as retained evidence; on success the root handle is closed. It
// never panics.
func RetireManagedOwner(owner *ManagedOwner, logger *slog.Logger) error {
	if owner == nil {
		return nil
	}
	if owner.owner == nil || owner.root == nil {
		return owner.Close()
	}
	if err := owner.owner.BeginClosing(); err != nil {
		return managedAcquireError(err)
	}
	if err := owner.owner.Retire(); err != nil {
		return managedAcquireError(err)
	}
	if logger != nil {
		logger.Info("managed ownership retired")
	}
	return owner.Close()
}

// Directory returns the canonical physical directory admitted by the
// acquisition (symlinks resolved).
func (o *ManagedOwner) Directory() string {
	if o == nil {
		return ""
	}
	return o.directory
}

// Validate re-proves that this owner still holds its published record and lock.
func (o *ManagedOwner) Validate() error {
	if o == nil || o.owner == nil {
		return errors.New("managed owner is not acquired")
	}
	return o.owner.Validate()
}

// Close releases the retained root handle only. It never removes the lock and
// is idempotent.
func (o *ManagedOwner) Close() error {
	if o == nil {
		return nil
	}
	if o.root == nil {
		return nil
	}
	return o.root.Close()
}

// managedClassification names the managed-state sentinel for operator-facing
// error text. Unknown errors are reported as IOFailure.
func managedClassification(err error) string {
	switch {
	case errors.Is(err, managedstate.ErrBusy):
		return "Busy"
	case errors.Is(err, managedstate.ErrTimeout):
		return "Timeout"
	case errors.Is(err, managedstate.ErrChangedRoot):
		return "ChangedRoot"
	case errors.Is(err, managedstate.ErrForeignState):
		return "ForeignState"
	case errors.Is(err, managedstate.ErrInvalidRecord):
		return "InvalidRecord"
	case errors.Is(err, managedstate.ErrUnknownAuthority):
		return "UnknownAuthority"
	case errors.Is(err, managedstate.ErrRetainedEvidence):
		return "RetainedEvidence"
	case errors.Is(err, managedstate.ErrRandomFailure):
		return "RandomFailure"
	default:
		return "IOFailure"
	}
}

func managedAcquireError(err error) error {
	return fmt.Errorf("managed ownership %s: %w", managedClassification(err), err)
}
