package main

// S7B1 bounded live-reprint transaction command.
//
// `managed-state reprint` performs the whole live-reprint transaction as one
// bounded operation: validate the request, refuse on retained journal evidence,
// require an active owner, acquire only T, stage the origin write with the S7A
// journal, arm the bootstrap invitation over the local pairing-control socket,
// then apply on a clear acknowledgement, roll back on a definite rejection, or
// retain the staged journal and fail closed on an ambiguous acknowledgement.
//
// The command never acquires, adopts or retires O (serve holds it), never
// removes foreign state, never retries and never performs automatic recovery.
// A lost acknowledgement leaves exactly one durable staged journal that blocks
// later reprints until the documented recovery runs.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/localcontrol"
	"github.com/0cv/herdr-mobile-relay/internal/managedstate"
)

const (
	managedReprintDefaultOriginFile = "phone-app-origin-configured"
	managedReprintDefaultDeadline   = 60 * time.Second
	managedReprintMinDeadline       = 1 * time.Second
	managedReprintMaxDeadline       = 60 * time.Second
	managedReprintAcquireLimit      = 5 * time.Second
	managedReprintArmTimeout        = localcontrol.ArmTimeout
	managedReprintMaxOriginValue    = 2048
	managedReprintMaxIdentity       = 128
)

const managedReprintUsage = "usage: herdr-mobile-relay managed-state reprint --dir DIR --socket PATH --run-id ID --instance ID [--origin-file phone-app-origin-configured] --origin-value VALUE [--deadline 60s]"

// reprintArmOutcome is the three-way classification of the arm IPC result.
type reprintArmOutcome int

const (
	// reprintArmAcknowledged means the relay durably armed a fresh invitation.
	reprintArmAcknowledged reprintArmOutcome = iota
	// reprintArmRejected means the relay answered and definitely did not arm.
	reprintArmRejected
	// reprintArmUncertain means no trustworthy answer was received.
	reprintArmUncertain
)

// runManagedReprint implements `managed-state reprint`. It writes only the
// single success JSON line to stdout and never returns 0 unless the origin
// write has been durably applied.
func runManagedReprint(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("managed-state reprint", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dir := flags.String("dir", "", "absolute canonical config root")
	socket := flags.String("socket", "", "absolute pairing control socket path")
	runID := flags.String("run-id", "", "managed foreground run identifier")
	instance := flags.String("instance", "", "relay instance identifier")
	originFile := flags.String("origin-file", managedReprintDefaultOriginFile, "managed origin file name")
	originValue := flags.String("origin-value", "", "new origin value")
	deadline := flags.Duration("deadline", managedReprintDefaultDeadline, "overall transaction deadline (1-60s)")
	if err := flags.Parse(args); err != nil {
		fmt.Fprintln(stderr, managedReprintUsage)
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, managedReprintUsage)
		return 2
	}
	if err := validateReprintFlags(*dir, *socket, *runID, *instance, *originFile, *originValue, *deadline); err != nil {
		fmt.Fprintf(stderr, "managed-state reprint: %v\n%s\n", err, managedReprintUsage)
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), *deadline)
	defer cancel()

	root, err := managedstate.OpenExistingRoot(*dir)
	if err != nil {
		fmt.Fprintf(stderr, "managed-state reprint: open root: %v\n", err)
		return 4
	}
	defer root.Close()

	// A journal already on disk is retained evidence for recovery. It is never
	// deleted, overwritten or auto-recovered here.
	status, err := managedstate.RecoverJournal(root)
	if err != nil {
		fmt.Fprintf(stderr, "managed-state reprint: inspect journal: %v\n", err)
		return 4
	}
	if status.Present {
		fmt.Fprintf(stderr, "managed-state reprint: a staged journal is present for this root; run the documented recovery before reprinting\n")
		return 4
	}

	// O is held by the live serve process. Reprint must prove an active owner
	// exists but must never acquire, adopt or retire it.
	ownerInfo, err := os.Lstat(filepath.Join(*dir, "owner.lock"))
	if err != nil || ownerInfo.Mode()&os.ModeSymlink != 0 || !ownerInfo.IsDir() {
		fmt.Fprintln(stderr, "managed-state reprint: no active owner lock; refusing without acquiring ownership")
		return 4
	}

	txn, err := root.AcquireTransaction(ctx, managedReprintAcquireLimit)
	if err != nil {
		fmt.Fprintf(stderr, "managed-state reprint: acquire transaction: %v\n", err)
		return managedReprintContentionCode(err)
	}
	defer txn.Release()

	entry, err := captureReprintEntry(*dir, *originFile, *originValue)
	if err != nil {
		fmt.Fprintf(stderr, "managed-state reprint: %v\n", err)
		return 4
	}
	journal, err := txn.BeginJournal(*runID, *runID, []managedstate.JournalEntry{entry})
	if err != nil {
		fmt.Fprintf(stderr, "managed-state reprint: begin journal: %v\n", err)
		return 4
	}

	response, armErr := armBootstrapReprint(ctx, *socket, *runID, *instance)

	switch classifyReprintArm(response, armErr) {
	case reprintArmAcknowledged:
		if err := journal.Apply(); err != nil {
			fmt.Fprintf(stderr, "managed-state reprint: apply journal: %v; the retained journal must be inspected before retrying\n", err)
			return 4
		}
		encoded, err := json.Marshal(struct {
			OK                  bool   `json:"ok"`
			InvitationExpiresAt string `json:"invitation_expires_at"`
		}{OK: true, InvitationExpiresAt: response.InvitationExpiresAt})
		if err != nil {
			fmt.Fprintf(stderr, "managed-state reprint: encode acknowledgement: %v\n", err)
			return 4
		}
		fmt.Fprintln(stdout, string(encoded))
		return 0
	case reprintArmRejected:
		if err := journal.Rollback(); err != nil {
			fmt.Fprintf(stderr, "managed-state reprint: rollback journal: %v; the retained journal must be inspected before retrying\n", err)
			return 4
		}
		fmt.Fprintln(stderr, "managed-state reprint: pairing control rejected the bootstrap invitation; the origin change was rolled back")
		return 3
	default:
		fmt.Fprintf(stderr, "managed-state reprint: the bootstrap invitation state is uncertain (%v); the staged journal is retained and later reprints are blocked until the documented recovery runs\n", armErr)
		return 6
	}
}

// armBootstrapReprint performs the arm IPC under the lifecycle operation bound
// and the overall transaction context.
func armBootstrapReprint(ctx context.Context, socket, runID, instance string) (localcontrol.Response, error) {
	armCtx, cancel := context.WithTimeout(ctx, managedReprintArmTimeout)
	defer cancel()
	return localcontrol.Request(armCtx, socket, "arm_bootstrap", runID, instance)
}

// classifyReprintArm decides whether the arm result is a clear
// acknowledgement, a definite rejection or an uncertain outcome.
//
// localcontrol.Request returns a non-nil error both for transport failures and
// for a decoded `ok:false` response. A decoded response always carries the
// server's error text (the pairing-control server sets `error` on every
// rejection), so a non-empty response.Error is a definite rejection while a
// non-nil error with an empty response is an ambiguous transport outcome.
func classifyReprintArm(response localcontrol.Response, err error) reprintArmOutcome {
	if response.Error != "" {
		return reprintArmRejected
	}
	if err != nil {
		return reprintArmUncertain
	}
	if !response.OK || !response.InvitationArmed || response.InvitationExpiresAt == "" {
		return reprintArmRejected
	}
	return reprintArmAcknowledged
}

// captureReprintEntry snapshots the origin file's prior bytes/absence/mode and
// sets the requested new content. The new file is always installed at 0600.
func captureReprintEntry(dir, originFile, originValue string) (managedstate.JournalEntry, error) {
	entry := managedstate.JournalEntry{
		Path:     originFile,
		NewBytes: []byte(originValue),
		NewMode:  0o600,
	}
	target := filepath.Join(dir, originFile)
	info, err := os.Lstat(target)
	if os.IsNotExist(err) {
		return entry, nil
	}
	if err != nil {
		return managedstate.JournalEntry{}, fmt.Errorf("inspect origin file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return managedstate.JournalEntry{}, errors.New("origin file is not a regular file")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return managedstate.JournalEntry{}, fmt.Errorf("read origin file: %w", err)
	}
	entry.PriorExists = true
	entry.PriorBytes = data
	entry.PriorMode = info.Mode().Perm()
	return entry, nil
}

// managedReprintContentionCode maps transaction acquisition failures to the
// fixed contract: contention (Busy/Timeout) is 3, anything else is 4.
func managedReprintContentionCode(err error) int {
	if errors.Is(err, managedstate.ErrBusy) || errors.Is(err, managedstate.ErrTimeout) {
		return 3
	}
	return 4
}

// validateReprintFlags performs every pure validation before any filesystem
// mutation, so a usage error can never change managed state.
func validateReprintFlags(dir, socket, runID, instance, originFile, originValue string, deadline time.Duration) error {
	if dir == "" || !filepath.IsAbs(dir) {
		return errors.New("--dir must be an absolute path")
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return errors.New("--dir must be an existing directory")
	}
	if socket == "" || !filepath.IsAbs(socket) {
		return errors.New("--socket must be an absolute path")
	}
	if !validReprintIdentity(runID) {
		return errors.New("--run-id must be 1-128 printable non-space ASCII characters without separators")
	}
	if !validReprintIdentity(instance) {
		return errors.New("--instance must be 1-128 printable non-space ASCII characters without separators")
	}
	if !validReprintOriginFile(originFile) {
		return errors.New("--origin-file must be a safe single base name")
	}
	if originValue == "" {
		return errors.New("--origin-value must not be empty")
	}
	if len(originValue) > managedReprintMaxOriginValue {
		return errors.New("--origin-value must be at most 2048 bytes")
	}
	if strings.ContainsAny(originValue, "\x00\n\r") {
		return errors.New("--origin-value must not contain a newline or NUL")
	}
	if deadline < managedReprintMinDeadline || deadline > managedReprintMaxDeadline {
		return errors.New("--deadline must be between 1s and 60s")
	}
	return nil
}

func validReprintIdentity(value string) bool {
	if value == "" || len(value) > managedReprintMaxIdentity {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c < 0x21 || c > 0x7e || c == '/' || c == '\\' {
			return false
		}
	}
	return true
}

// validReprintOriginFile accepts one safe base name that cannot collide with
// the managed root's control objects or the journal's own staged files.
func validReprintOriginFile(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, "\x00/\\") {
		return false
	}
	if filepath.Base(name) != name || filepath.Clean(name) != name {
		return false
	}
	switch name {
	case "owner.lock", "txn.lock", "owner.json", "journal.json", "generation":
		return false
	}
	if strings.HasPrefix(name, "journal.stage.") {
		return false
	}
	return true
}
