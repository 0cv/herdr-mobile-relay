package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/localcontrol"
	"github.com/0cv/herdr-mobile-relay/internal/managedstate"
)

// reprintOriginFile mirrors the command's default origin file name without
// referencing any identifier that the S7B1 mutant overlay does not define.
const reprintOriginFile = "phone-app-origin-configured"

func TestClassifyReprintArmRetainsUnresolvedInvitationJournal(t *testing.T) {
	response := localcontrol.Response{Error: "durable arm rollback requires recovery", ArmOutcome: "unresolved"}
	if got := classifyReprintArm(response, nil); got != reprintArmUncertain {
		t.Fatalf("unresolved arm classified as %v, want uncertain", got)
	}
	response.ArmOutcome = "committed"
	if got := classifyReprintArm(response, nil); got != reprintArmUncertain {
		t.Fatalf("committed but negatively acknowledged arm classified as %v, want uncertain", got)
	}
	response = localcontrol.Response{
		OK: true, ArmOutcome: "committed", InvitationArmed: true, InvitationExpiresAt: "fixture-expiry",
	}
	if got := classifyReprintArm(response, nil); got != reprintArmAcknowledged {
		t.Fatalf("durably committed arm acknowledgement classified as %v, want acknowledged", got)
	}
	response = localcontrol.Response{Error: "definite refusal", ArmOutcome: "not-committed"}
	if got := classifyReprintArm(response, nil); got != reprintArmRejected {
		t.Fatalf("definitely refused arm classified as %v, want rejected", got)
	}
}

func TestSafeArmFailureCodeFiltersUnknownValues(t *testing.T) {
	if got := safeArmFailureCode("local_readiness_incomplete"); got != "local_readiness_incomplete" {
		t.Fatalf("known failure code = %q", got)
	}
	if got := safeArmFailureCode("secret https://private.example.test/token"); got != "" {
		t.Fatalf("unknown failure detail escaped filter: %q", got)
	}
}

// reprintOwnedRoot creates a private 0700 canonical root with a published B1
// owner so the reprint command's active-owner check passes.
func reprintOwnedRoot(t *testing.T) string {
	t.Helper()
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
	return dir
}

// shortReprintDir makes a short-lived directory whose path is small enough for
// a Unix socket pathname (the sandbox TMPDIR is longer than the sun_path
// limit), so the fake control server can bind a real socket.
func shortReprintDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "s7b1-")
	if err != nil {
		t.Fatalf("create short temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// startReprintControlServer serves the localcontrol JSON-line protocol over a
// real Unix socket and answers every request with response.
func startReprintControlServer(t *testing.T, response string) string {
	t.Helper()
	socket := filepath.Join(shortReprintDir(t), "control.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen fake control socket: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go func(connection net.Conn) {
				defer connection.Close()
				_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
				_, _ = bufio.NewReader(connection).ReadString('\n')
				_, _ = io.WriteString(connection, response+"\n")
			}(connection)
		}
	}()
	return socket
}

func startControllableReprintControlServer(t *testing.T, response string) (string, <-chan struct{}, func(), <-chan struct{}) {
	t.Helper()
	socket := filepath.Join(shortReprintDir(t), "control.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen delayed fake control socket: %v", err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	clientClosed := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() {
		unblock()
		_ = listener.Close()
	})
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer connection.Close()
		_ = connection.SetDeadline(time.Now().Add(10 * time.Second))
		if _, err := bufio.NewReader(connection).ReadString('\n'); err != nil {
			return
		}
		close(started)
		go func() {
			var extra [1]byte
			_, _ = connection.Read(extra[:])
			close(clientClosed)
		}()
		select {
		case <-release:
			_, _ = io.WriteString(connection, response+"\n")
		case <-clientClosed:
		case <-time.After(10 * time.Second):
		}
	}()
	return socket, started, unblock, clientClosed
}

// stageReprintJournal leaves a real staged S7A journal on the root and releases
// T so the command can observe retained evidence.
func stageReprintJournal(t *testing.T, dir, runID, originFile string, prior, next []byte) {
	t.Helper()
	root, err := managedstate.OpenExistingRoot(dir)
	if err != nil {
		t.Fatalf("OpenExistingRoot: %v", err)
	}
	txn, err := root.AcquireTransaction(context.Background(), time.Second)
	if err != nil {
		t.Fatalf("AcquireTransaction: %v", err)
	}
	entry := managedstate.JournalEntry{Path: originFile, NewBytes: next, NewMode: 0o600}
	if prior != nil {
		entry.PriorExists = true
		entry.PriorBytes = prior
		entry.PriorMode = 0o600
	}
	if _, err := txn.BeginJournal(runID, runID, []managedstate.JournalEntry{entry}); err != nil {
		t.Fatalf("BeginJournal: %v", err)
	}
	if err := txn.Release(); err != nil {
		t.Fatalf("release staged transaction: %v", err)
	}
	if err := root.Close(); err != nil {
		t.Fatalf("close staged root: %v", err)
	}
}

func readReprintFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func assertNoReprintJournal(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(dir, "journal.json")); !os.IsNotExist(err) {
		t.Fatalf("journal.json present after terminal transaction: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read root: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "journal.stage.") {
			t.Fatalf("staged journal file remained: %s", entry.Name())
		}
	}
}

func assertNoReprintTransactionLock(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(dir, "txn.lock")); !os.IsNotExist(err) {
		t.Fatalf("txn.lock present after command returned: %v", err)
	}
}

func TestManagedReprintBudgetCoversArmAcknowledgement(t *testing.T) {
	if managedReprintArmTimeout != localcontrol.ArmTimeout {
		t.Fatalf("reprint arm timeout = %s, control arm timeout = %s", managedReprintArmTimeout, localcontrol.ArmTimeout)
	}
	if managedReprintDefaultDeadline <= localcontrol.ArmTimeout || managedReprintMaxDeadline <= localcontrol.ArmTimeout {
		t.Fatalf("reprint transaction bounds (default=%s max=%s) do not include the full arm operation (%s)", managedReprintDefaultDeadline, managedReprintMaxDeadline, localcontrol.ArmTimeout)
	}
}

func TestManagedStateReprintAppliesOriginAndAcks(t *testing.T) {
	dir := reprintOwnedRoot(t)
	origin := filepath.Join(dir, reprintOriginFile)
	if err := os.WriteFile(origin, []byte("https://old.example"), 0o600); err != nil {
		t.Fatalf("write prior origin: %v", err)
	}
	socket := startReprintControlServer(t, `{"ok":true,"invitation_armed":true,"invitation_expires_at":"2026-06-01T00:00:00Z"}`)

	var stdout, stderr bytes.Buffer
	code := runManagedReprint([]string{
		"--dir", dir, "--socket", socket,
		"--run-id", "run-1", "--instance", "instance-1",
		"--origin-value", "https://new.example",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr=%s)", code, stderr.String())
	}
	if got := string(readReprintFile(t, origin)); got != "https://new.example" {
		t.Fatalf("origin = %q, want new value", got)
	}
	info, err := os.Stat(origin)
	if err != nil {
		t.Fatalf("stat origin: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("origin mode = %o, want 600", info.Mode().Perm())
	}
	assertNoReprintJournal(t, dir)
	assertNoReprintTransactionLock(t, dir)
	want := "{\"ok\":true,\"invitation_expires_at\":\"2026-06-01T00:00:00Z\"}\n"
	if stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestManagedStateReprintWaitsForDelayedArmAcknowledgementWithinBudget(t *testing.T) {
	dir := reprintOwnedRoot(t)
	origin := filepath.Join(dir, reprintOriginFile)
	if err := os.WriteFile(origin, []byte("https://old.example"), 0o600); err != nil {
		t.Fatalf("write prior origin: %v", err)
	}
	socket, started, release, _ := startControllableReprintControlServer(t, `{"ok":true,"invitation_armed":true,"invitation_expires_at":"2026-06-01T00:00:00Z"}`)
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- runManagedReprint([]string{
			"--dir", dir, "--socket", socket,
			"--run-id", "run-delayed", "--instance", "instance-delayed",
			"--origin-value", "https://new.example",
		}, &stdout, &stderr)
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("reprint did not reach the deliberately delayed arm acknowledgement")
	}
	release()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("delayed reprint exit = %d (stderr=%s)", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reprint did not complete after delayed arm acknowledgement")
	}
	if got := string(readReprintFile(t, origin)); got != "https://new.example" {
		t.Fatalf("delayed reprint origin = %q, want new value", got)
	}
	assertNoReprintJournal(t, dir)
}

func TestManagedStateReprintCancellationRetainsStagedJournal(t *testing.T) {
	dir := reprintOwnedRoot(t)
	origin := filepath.Join(dir, reprintOriginFile)
	if err := os.WriteFile(origin, []byte("https://old.example"), 0o600); err != nil {
		t.Fatalf("write prior origin: %v", err)
	}
	socket, started, _, clientClosed := startControllableReprintControlServer(t, "")
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- runManagedReprint([]string{
			"--dir", dir, "--socket", socket,
			"--run-id", "run-canceled", "--instance", "instance-canceled",
			"--origin-value", "https://new.example", "--deadline", "3s",
		}, &stdout, &stderr)
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("reprint did not reach the delayed arm acknowledgement")
	}
	select {
	case code := <-done:
		if code != 6 {
			t.Fatalf("canceled reprint exit = %d, want uncertain outcome 6 (stderr=%s)", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reprint did not honor its bounded transaction cancellation")
	}
	select {
	case <-clientClosed:
	case <-time.After(time.Second):
		t.Fatal("reprint cancellation did not close the pending control request")
	}
	if got := string(readReprintFile(t, origin)); got != "https://old.example" {
		t.Fatalf("canceled reprint changed origin to %q", got)
	}
	root, err := managedstate.OpenExistingRoot(dir)
	if err != nil {
		t.Fatalf("OpenExistingRoot after cancellation: %v", err)
	}
	status, err := managedstate.RecoverJournal(root)
	if err != nil {
		t.Fatalf("RecoverJournal after cancellation: %v", err)
	}
	if !status.Present || status.State != "staged" {
		t.Fatalf("canceled reprint journal = %+v, want present staged", status)
	}
	if err := root.Close(); err != nil {
		t.Fatalf("close root after cancellation: %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("canceled reprint stdout = %q, want empty", stdout.String())
	}
}

func TestManagedStateReprintRejectsWithExistingJournal(t *testing.T) {
	dir := reprintOwnedRoot(t)
	origin := filepath.Join(dir, reprintOriginFile)
	if err := os.WriteFile(origin, []byte("https://old.example"), 0o600); err != nil {
		t.Fatalf("write prior origin: %v", err)
	}
	stageReprintJournal(t, dir, "run-2", reprintOriginFile, []byte("https://old.example"), []byte("https://new.example"))

	var stdout, stderr bytes.Buffer
	code := runManagedReprint([]string{
		"--dir", dir, "--socket", filepath.Join(dir, "unused.sock"),
		"--run-id", "run-2", "--instance", "instance-2",
		"--origin-value", "https://new.example",
	}, &stdout, &stderr)
	if code != 4 {
		t.Fatalf("exit = %d, want 4 (stderr=%s)", code, stderr.String())
	}
	if got := string(readReprintFile(t, origin)); got != "https://old.example" {
		t.Fatalf("origin = %q, want prior bytes", got)
	}
	root, err := managedstate.OpenExistingRoot(dir)
	if err != nil {
		t.Fatalf("OpenExistingRoot: %v", err)
	}
	defer root.Close()
	status, err := managedstate.RecoverJournal(root)
	if err != nil {
		t.Fatalf("RecoverJournal: %v", err)
	}
	if !status.Present {
		t.Fatalf("journal was not retained")
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

func TestManagedStateReprintArmRejectionRollsBack(t *testing.T) {
	dir := reprintOwnedRoot(t)
	origin := filepath.Join(dir, reprintOriginFile)
	if err := os.WriteFile(origin, []byte("https://old.example"), 0o644); err != nil {
		t.Fatalf("write prior origin: %v", err)
	}
	socket := startReprintControlServer(t, `{"ok":false,"error":"bootstrap invitation could not be persisted"}`)

	var stdout, stderr bytes.Buffer
	code := runManagedReprint([]string{
		"--dir", dir, "--socket", socket,
		"--run-id", "run-3", "--instance", "instance-3",
		"--origin-value", "https://new.example",
	}, &stdout, &stderr)
	if code != 3 {
		t.Fatalf("exit = %d, want 3 (stderr=%s)", code, stderr.String())
	}
	if got := string(readReprintFile(t, origin)); got != "https://old.example" {
		t.Fatalf("origin = %q, want prior bytes", got)
	}
	info, err := os.Stat(origin)
	if err != nil {
		t.Fatalf("stat origin: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("origin mode = %o, want prior 644", info.Mode().Perm())
	}
	assertNoReprintJournal(t, dir)
	assertNoReprintTransactionLock(t, dir)
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

func TestManagedReprintDeadlineCoversArmBudgetAndOwnerAcquisition(t *testing.T) {
	minimumSafeDeadline := managedReprintArmTimeout + managedReprintAcquireLimit
	if managedReprintDefaultDeadline < minimumSafeDeadline || managedReprintMaxDeadline < minimumSafeDeadline {
		t.Fatalf("reprint default/max budgets %s/%s do not cover arm %s plus owner acquisition %s", managedReprintDefaultDeadline, managedReprintMaxDeadline, managedReprintArmTimeout, managedReprintAcquireLimit)
	}
}

func TestManagedStateReprintAmbiguousRetainsJournal(t *testing.T) {
	dir := reprintOwnedRoot(t)
	origin := filepath.Join(dir, reprintOriginFile)
	if err := os.WriteFile(origin, []byte("https://old.example"), 0o600); err != nil {
		t.Fatalf("write prior origin: %v", err)
	}
	socket := filepath.Join(shortReprintDir(t), "missing.sock")

	var stdout, stderr bytes.Buffer
	code := runManagedReprint([]string{
		"--dir", dir, "--socket", socket,
		"--run-id", "run-4", "--instance", "instance-4",
		"--origin-value", "https://new.example",
	}, &stdout, &stderr)
	if code != 6 {
		t.Fatalf("exit = %d, want 6 (stderr=%s)", code, stderr.String())
	}
	if got := string(readReprintFile(t, origin)); got != "https://old.example" {
		t.Fatalf("origin = %q, want prior bytes", got)
	}
	root, err := managedstate.OpenExistingRoot(dir)
	if err != nil {
		t.Fatalf("OpenExistingRoot: %v", err)
	}
	status, err := managedstate.RecoverJournal(root)
	if err != nil {
		t.Fatalf("RecoverJournal: %v", err)
	}
	if !status.Present || status.State != "staged" {
		t.Fatalf("journal status = %+v, want present staged", status)
	}
	if err := root.Close(); err != nil {
		t.Fatalf("close root: %v", err)
	}
	assertNoReprintTransactionLock(t, dir)

	var secondOut, secondErr bytes.Buffer
	second := runManagedReprint([]string{
		"--dir", dir, "--socket", socket,
		"--run-id", "run-4", "--instance", "instance-4",
		"--origin-value", "https://new.example",
	}, &secondOut, &secondErr)
	if second != 4 {
		t.Fatalf("second exit = %d, want 4 (stderr=%s)", second, secondErr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

func TestManagedStateReprintRequiresSafeFlags(t *testing.T) {
	dir := reprintOwnedRoot(t)
	absoluteSocket := filepath.Join(shortReprintDir(t), "unused.sock")
	base := []string{"--run-id", "run-5", "--instance", "instance-5", "--origin-value", "https://new.example"}
	cases := []struct {
		name string
		args []string
	}{
		{"nested origin file", append([]string{"--dir", dir, "--socket", absoluteSocket, "--origin-file", "sub/origin"}, base...)},
		{"reserved owner.lock", append([]string{"--dir", dir, "--socket", absoluteSocket, "--origin-file", "owner.lock"}, base...)},
		{"reserved txn.lock", append([]string{"--dir", dir, "--socket", absoluteSocket, "--origin-file", "txn.lock"}, base...)},
		{"reserved owner.json", append([]string{"--dir", dir, "--socket", absoluteSocket, "--origin-file", "owner.json"}, base...)},
		{"reserved journal.json", append([]string{"--dir", dir, "--socket", absoluteSocket, "--origin-file", "journal.json"}, base...)},
		{"reserved generation", append([]string{"--dir", dir, "--socket", absoluteSocket, "--origin-file", "generation"}, base...)},
		{"oversized value", append([]string{"--dir", dir, "--socket", absoluteSocket}, "--run-id", "run-5", "--instance", "instance-5", "--origin-value", strings.Repeat("a", 2049))},
		{"missing run id", []string{"--dir", dir, "--socket", absoluteSocket, "--instance", "instance-5", "--origin-value", "https://new.example"}},
		{"missing instance", []string{"--dir", dir, "--socket", absoluteSocket, "--run-id", "run-5", "--origin-value", "https://new.example"}},
		{"non-absolute dir", append([]string{"--dir", "relative-root", "--socket", absoluteSocket}, base...)},
		{"non-absolute socket", append([]string{"--dir", dir, "--socket", "relative.sock"}, base...)},
		{"extra argument", append(append([]string{"--dir", dir, "--socket", absoluteSocket}, base...), "extra")},
	}
	for _, tc := range cases {
		before := snapshotTree(t, dir)
		var stdout, stderr bytes.Buffer
		if code := runManagedReprint(tc.args, &stdout, &stderr); code != 2 {
			t.Fatalf("%s: exit = %d, want 2 (stderr=%s)", tc.name, code, stderr.String())
		}
		if stdout.Len() != 0 {
			t.Fatalf("%s: stdout = %q, want empty", tc.name, stdout.String())
		}
		if after := snapshotTree(t, dir); !reflect.DeepEqual(before, after) {
			t.Fatalf("%s: usage refusal changed the filesystem: before=%v after=%v", tc.name, before, after)
		}
	}
}

func TestManagedStateReprintMissingOwnerLockRefused(t *testing.T) {
	dir := managedStateRoot(t)
	before := snapshotTree(t, dir)
	socket := startReprintControlServer(t, `{"ok":true,"invitation_armed":true,"invitation_expires_at":"2026-06-01T00:00:00Z"}`)

	var stdout, stderr bytes.Buffer
	code := runManagedReprint([]string{
		"--dir", dir, "--socket", socket,
		"--run-id", "run-6", "--instance", "instance-6",
		"--origin-value", "https://new.example",
	}, &stdout, &stderr)
	if code != 4 {
		t.Fatalf("exit = %d, want 4 (stderr=%s)", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if after := snapshotTree(t, dir); !reflect.DeepEqual(before, after) {
		t.Fatalf("missing-owner refusal changed the filesystem: before=%v after=%v", before, after)
	}
}
