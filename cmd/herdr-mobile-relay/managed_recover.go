package main

// S7C stopped-owner journal recovery command.
//
// `managed-state recover` reconciles exactly one retained S7A journal after the
// operator has stopped the relay. It never acquires, adopts or retires the
// owner lock and never takes over a live or crashed owner. It prints one JSON
// line on success and makes no claim that an invitation was not persisted.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/managedstate"
)

const (
	managedRecoverDefaultTimeout = 5 * time.Second
	managedRecoverMinTimeout     = 1 * time.Second
	managedRecoverMaxTimeout     = 60 * time.Second
)

const managedRecoverUsage = "usage: herdr-mobile-relay managed-state recover --dir DIR [--timeout 5s]"

// managedRecoverOutput is the single success JSON line. The absent-journal
// no-op is emitted separately as exactly {"ok":true,"present":false}.
type managedRecoverOutput struct {
	OK             bool   `json:"ok"`
	Present        bool   `json:"present"`
	State          string `json:"state"`
	Entries        int    `json:"entries"`
	Restored       bool   `json:"restored"`
	RemovedJournal bool   `json:"removed_journal"`
	RemovedStaging bool   `json:"removed_staging"`
}

// runManagedRecover implements `managed-state recover`. It writes at most one
// JSON line to stdout and never returns 0 unless the recovery succeeded or
// there was nothing to recover.
func runManagedRecover(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("managed-state recover", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dir := flags.String("dir", "", "absolute canonical config root")
	timeout := flags.Duration("timeout", managedRecoverDefaultTimeout, "bounded recovery timeout (1-60s)")
	if err := flags.Parse(args); err != nil {
		fmt.Fprintln(stderr, managedRecoverUsage)
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, managedRecoverUsage)
		return 2
	}
	if err := validateRecoverFlags(*dir, *timeout); err != nil {
		fmt.Fprintf(stderr, "managed-state recover: %v\n%s\n", err, managedRecoverUsage)
		return 2
	}

	// The command timeout bounds the whole operation; the API additionally
	// bounds transaction acquisition.
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	root, err := managedstate.OpenExistingRoot(*dir)
	if err != nil {
		fmt.Fprintf(stderr, "managed-state recover: open root: %v\n", err)
		return 4
	}
	defer root.Close()

	outcome, err := managedstate.RecoverStagedJournal(ctx, root)
	if err != nil {
		return managedRecoverErrorCode(err, stderr)
	}
	if !outcome.Present {
		fmt.Fprintln(stdout, `{"ok":true,"present":false}`)
		return 0
	}
	encoded, err := json.Marshal(managedRecoverOutput{
		OK:             true,
		Present:        true,
		State:          outcome.State,
		Entries:        outcome.Entries,
		Restored:       outcome.Restored,
		RemovedJournal: outcome.RemovedJournal,
		RemovedStaging: outcome.RemovedStaging,
	})
	if err != nil {
		fmt.Fprintf(stderr, "managed-state recover: encode outcome: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, string(encoded))
	return 0
}

// managedRecoverErrorCode maps API failures to the fixed contract: contention
// is 3, retained or invalid evidence is 4, anything else is 1.
func managedRecoverErrorCode(err error, stderr io.Writer) int {
	switch {
	case errors.Is(err, managedstate.ErrBusy):
		fmt.Fprintln(stderr, "managed-state recover: an owner lock is present; stop the relay and retry; nothing was changed")
		return 3
	case errors.Is(err, managedstate.ErrForeignState),
		errors.Is(err, managedstate.ErrInvalidRecord),
		errors.Is(err, managedstate.ErrRetainedEvidence),
		errors.Is(err, managedstate.ErrUnknownAuthority):
		fmt.Fprintln(stderr, "managed-state recover: the retained journal could not be safely reconciled; inspect the retained journal")
		return 4
	default:
		fmt.Fprintf(stderr, "managed-state recover: %v\n", err)
		return 1
	}
}

// validateRecoverFlags performs every pure validation before any managed state
// is opened, so a usage error can never change managed state.
func validateRecoverFlags(dir string, timeout time.Duration) error {
	if dir == "" || !filepath.IsAbs(dir) {
		return errors.New("--dir must be an absolute path")
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return errors.New("--dir must be an existing directory")
	}
	if timeout < managedRecoverMinTimeout || timeout > managedRecoverMaxTimeout {
		return errors.New("--timeout must be between 1s and 60s")
	}
	return nil
}
