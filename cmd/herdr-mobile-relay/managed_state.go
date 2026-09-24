package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/managedstate"
)

// The managed-state holder owns the real O/T handles for the lifetime of the
// command while a cooperating shell mutates managed state. It is a bounded,
// interim, S9-aligned mechanism: it holds the lock, releases it cleanly on
// SIGTERM, and retains it (fail closed) on every other outcome. It performs no
// child supervision and no request authorization over a channel.

const (
	managedStateDefaultTimeout = 5 * time.Second
	managedStateMaxTimeout     = 60 * time.Second

	managedStateUsage = "usage: herdr-mobile-relay managed-state hold --dir DIR --operation owner|transaction [--timeout DURATION]"
)

// runManagedState implements `managed-state hold`. Output, signal handling,
// parent-death polling and the exit-code contract are fixed by S9A.
func runManagedState(args []string, stdout, stderr io.Writer, signals <-chan os.Signal, ppid func() int, tick time.Duration) int {
	if len(args) == 0 || args[0] != "hold" {
		fmt.Fprintln(stderr, managedStateUsage)
		return 2
	}
	flags := flag.NewFlagSet("managed-state hold", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dir := flags.String("dir", "", "absolute canonical config root")
	operation := flags.String("operation", "", "owner or transaction")
	timeout := flags.Duration("timeout", managedStateDefaultTimeout, "transaction acquisition bound (0-60s)")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *dir == "" || !filepath.IsAbs(*dir) {
		fmt.Fprintln(stderr, managedStateUsage)
		return 2
	}
	if *operation != "owner" && *operation != "transaction" {
		fmt.Fprintf(stderr, "managed-state: --operation must be owner or transaction\n%s\n", managedStateUsage)
		return 2
	}
	if *timeout < 0 || *timeout > managedStateMaxTimeout {
		fmt.Fprintf(stderr, "managed-state: --timeout must be between 0 and %s\n", managedStateMaxTimeout)
		return 2
	}

	// Capture the parent identity before acquisition so an orphaned holder can
	// never block every later run after its launcher dies.
	startParent := ppid()

	root, err := managedstate.OpenExistingRoot(*dir)
	if err != nil {
		fmt.Fprintf(stderr, "managed-state: open root: %v\n", err)
		return managedStateErrorCode(err)
	}

	var owner *managedstate.Owner
	var txn *managedstate.Txn
	switch *operation {
	case "owner":
		owner, err = root.TryAcquireOwner()
		if err != nil {
			root.Close()
			fmt.Fprintf(stderr, "managed-state: acquire owner: %v\n", err)
			return managedStateErrorCode(err)
		}
		if err = owner.PublishRecord(); err != nil {
			owner.Close()
			fmt.Fprintf(stderr, "managed-state: publish owner record: %v\n", err)
			return managedStateErrorCode(err)
		}
		writeReadiness(stdout, "owner")
	case "transaction":
		txn, err = root.AcquireTransaction(context.Background(), *timeout)
		if err != nil {
			root.Close()
			fmt.Fprintf(stderr, "managed-state: acquire transaction: %v\n", err)
			return managedStateErrorCode(err)
		}
		writeReadiness(stdout, "transaction")
	}

	if tick <= 0 {
		tick = time.Millisecond
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	for {
		select {
		case sig, ok := <-signals:
			if !ok {
				fmt.Fprintln(stderr, "managed-state: signal channel closed; retaining lock")
				retainManagedState(owner, txn, root)
				return 5
			}
			if sig == syscall.SIGTERM {
				return releaseManagedState(owner, txn, root, stderr)
			}
			fmt.Fprintf(stderr, "managed-state: interrupted by %v; retaining lock\n", sig)
			retainManagedState(owner, txn, root)
			return 5
		case <-ticker.C:
			if ppid() != startParent {
				fmt.Fprintln(stderr, "managed-state: parent process changed; retaining lock")
				retainManagedState(owner, txn, root)
				return 5
			}
		}
	}
}

// writeReadiness emits the single readiness line and flushes it when stdout is
// buffered.
func writeReadiness(stdout io.Writer, operation string) {
	fmt.Fprintf(stdout, "{\"ok\":true,\"operation\":%q}\n", operation)
	if flusher, ok := stdout.(interface{ Flush() error }); ok {
		_ = flusher.Flush()
	}
}

// releaseManagedState performs the SIGTERM clean-release protocol and reports
// any failure through the exit-code taxonomy rather than silently succeeding.
func releaseManagedState(owner *managedstate.Owner, txn *managedstate.Txn, root *managedstate.Root, stderr io.Writer) int {
	if owner != nil {
		if err := owner.BeginClosing(); err != nil {
			fmt.Fprintf(stderr, "managed-state: begin closing: %v\n", err)
			owner.Close()
			return managedStateErrorCode(err)
		}
		if err := owner.Retire(); err != nil {
			fmt.Fprintf(stderr, "managed-state: retire owner: %v\n", err)
			owner.Close()
			return managedStateErrorCode(err)
		}
		owner.Close()
		return 0
	}
	if txn != nil {
		if err := txn.Release(); err != nil {
			fmt.Fprintf(stderr, "managed-state: release transaction: %v\n", err)
			root.Close()
			return managedStateErrorCode(err)
		}
		root.Close()
		return 0
	}
	root.Close()
	return 0
}

// retainManagedState closes handles without removing any lock. The owner's
// published record and the transaction directory are left in place as evidence.
func retainManagedState(owner *managedstate.Owner, txn *managedstate.Txn, root *managedstate.Root) {
	if owner != nil {
		owner.Close()
		return
	}
	_ = txn
	root.Close()
}

// managedStateErrorCode maps a managedstate sentinel to the fixed S9A exit
// codes: 3 for contention, 4 for fail-closed evidence, 1 otherwise.
func managedStateErrorCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, managedstate.ErrBusy), errors.Is(err, managedstate.ErrTimeout):
		return 3
	case errors.Is(err, managedstate.ErrChangedRoot),
		errors.Is(err, managedstate.ErrForeignState),
		errors.Is(err, managedstate.ErrInvalidRecord),
		errors.Is(err, managedstate.ErrUnknownAuthority),
		errors.Is(err, managedstate.ErrRetainedEvidence):
		return 4
	default:
		return 1
	}
}
