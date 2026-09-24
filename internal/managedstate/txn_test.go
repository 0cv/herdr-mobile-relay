package managedstate_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/managedstate"
)

// TestRootTransactionAcquiresWithoutOwner proves the T-only acquisition path
// works with no owner ever having been acquired, and that the nil-owner Txn
// release removes exactly its own lock.
func TestRootTransactionAcquiresWithoutOwner(t *testing.T) {
	dir := mkRoot(t)
	root := openRoot(t, dir)
	defer root.Close()

	if _, err := os.Lstat(filepath.Join(dir, "owner.lock")); !os.IsNotExist(err) {
		t.Fatalf("precondition: owner.lock exists (%v), want absent", err)
	}

	txn, err := root.AcquireTransaction(context.Background(), 0)
	noErr(t, err, "Root.AcquireTransaction")
	if txn == nil {
		t.Fatal("Root.AcquireTransaction returned a nil Txn")
	}
	lockPath := filepath.Join(dir, "txn.lock")
	assertMode(t, lockPath, 0o700)

	noErr(t, txn.Release(), "Txn.Release")
	if _, err := os.Lstat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("txn.lock still present after release: %v", err)
	}
}

// TestRootTransactionContendsWithOwnerHeldTransaction proves T is a single
// shared lock: while an owner holds it, the root-level acquirer times out, and
// once released the root-level acquirer succeeds.
func TestRootTransactionContendsWithOwnerHeldTransaction(t *testing.T) {
	dir := mkRoot(t)
	ownerRoot := openRoot(t, dir)
	defer ownerRoot.Close()
	owner := acquireOwner(t, ownerRoot)
	publishOwner(t, owner)
	held := acquireTxn(t, owner, 0)

	contender := openRoot(t, dir)
	defer contender.Close()
	if _, err := contender.AcquireTransaction(context.Background(), 0); !errors.Is(err, managedstate.ErrTimeout) {
		t.Fatalf("contended AcquireTransaction: got %v, want %v", err, managedstate.ErrTimeout)
	}

	noErr(t, held.Release(), "held Txn.Release")
	txn, err := contender.AcquireTransaction(context.Background(), 0)
	noErr(t, err, "AcquireTransaction after release")
	noErr(t, txn.Release(), "Txn.Release")
}

// TestRootTransactionTimeoutAndRelease proves a pre-existing txn.lock produces
// ErrTimeout and that removing it (simulating a cooperative owner) lets the
// acquisition and release complete.
func TestRootTransactionTimeoutAndRelease(t *testing.T) {
	dir := mkRoot(t)
	lockPath := filepath.Join(dir, "txn.lock")
	if err := os.Mkdir(lockPath, 0o700); err != nil {
		t.Fatalf("mkdir txn.lock: %v", err)
	}
	root := openRoot(t, dir)
	defer root.Close()

	if _, err := root.AcquireTransaction(context.Background(), 0); !errors.Is(err, managedstate.ErrTimeout) {
		t.Fatalf("stale AcquireTransaction: got %v, want %v", err, managedstate.ErrTimeout)
	}

	if err := os.Remove(lockPath); err != nil {
		t.Fatalf("remove txn.lock: %v", err)
	}
	txn, err := root.AcquireTransaction(context.Background(), 0)
	noErr(t, err, "AcquireTransaction after removal")
	noErr(t, txn.Release(), "Txn.Release")
	if _, err := os.Lstat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("txn.lock still present after release: %v", err)
	}
}
