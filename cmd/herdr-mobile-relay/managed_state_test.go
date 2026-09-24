package main

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/managedstate"
)

// managedStateRoot creates a private 0700 canonical root for holder tests.
func managedStateRoot(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "root")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	return dir
}

func constantPpid(value int) func() int {
	return func() int { return value }
}

func waitManagedState(t *testing.T, done <-chan int, within time.Duration) int {
	t.Helper()
	select {
	case code := <-done:
		return code
	case <-time.After(within):
		t.Fatalf("managed-state holder did not exit within %s", within)
		return -1
	}
}

func readLineWithin(t *testing.T, reader io.Reader, within time.Duration) string {
	t.Helper()
	lineCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(reader).ReadString('\n')
		if err != nil {
			errCh <- err
			return
		}
		lineCh <- line
	}()
	select {
	case line := <-lineCh:
		return line
	case err := <-errCh:
		t.Fatalf("read readiness: %v", err)
		return ""
	case <-time.After(within):
		t.Fatalf("timed out waiting for readiness line")
		return ""
	}
}

// snapshotTree records relative paths, modes and file contents so a refusal can
// be proven to have touched nothing.
func snapshotTree(t *testing.T, dir string) map[string]string {
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

func TestManagedStateHoldUsageAndRefusals(t *testing.T) {
	root := managedStateRoot(t)

	usageCases := []struct {
		name string
		args []string
	}{
		{"missing hold", nil},
		{"wrong subcommand", []string{"halt", "--dir", root, "--operation", "owner"}},
		{"missing flags", []string{"hold"}},
		{"missing operation", []string{"hold", "--dir", root}},
		{"unknown operation", []string{"hold", "--dir", root, "--operation", "bogus"}},
		{"relative dir", []string{"hold", "--dir", "relative", "--operation", "owner"}},
		{"extra argument", []string{"hold", "--dir", root, "--operation", "owner", "extra"}},
		{"timeout too large", []string{"hold", "--dir", root, "--operation", "owner", "--timeout", "61s"}},
		{"negative timeout", []string{"hold", "--dir", root, "--operation", "owner", "--timeout", "-1s"}},
	}
	for _, tc := range usageCases {
		before := snapshotTree(t, root)
		signals := make(chan os.Signal, 2)
		if code := runManagedState(tc.args, io.Discard, io.Discard, signals, constantPpid(1), time.Millisecond); code != 2 {
			t.Fatalf("%s: exit = %d, want 2", tc.name, code)
		}
		if after := snapshotTree(t, root); !reflect.DeepEqual(before, after) {
			t.Fatalf("%s: usage refusal changed the filesystem: before=%v after=%v", tc.name, before, after)
		}
	}

	// A root already owned by the B1 helper refuses with contention (3).
	owned := managedStateRoot(t)
	b1Root, err := managedstate.OpenExistingRoot(owned)
	if err != nil {
		t.Fatalf("OpenExistingRoot owned: %v", err)
	}
	defer b1Root.Close()
	owner, err := b1Root.TryAcquireOwner()
	if err != nil {
		t.Fatalf("TryAcquireOwner: %v", err)
	}
	if err := owner.PublishRecord(); err != nil {
		t.Fatalf("PublishRecord: %v", err)
	}
	defer owner.Close()

	before := snapshotTree(t, owned)
	if code := runManagedState([]string{"hold", "--dir", owned, "--operation", "owner"}, io.Discard, io.Discard, make(chan os.Signal, 2), constantPpid(1), time.Millisecond); code != 3 {
		t.Fatalf("owned root: exit = %d, want 3", code)
	}
	if after := snapshotTree(t, owned); !reflect.DeepEqual(before, after) {
		t.Fatalf("owned-root refusal changed the filesystem: before=%v after=%v", before, after)
	}

	// A non-directory foreign object at owner.lock fails closed (4).
	foreignFile := managedStateRoot(t)
	if err := os.WriteFile(filepath.Join(foreignFile, "owner.lock"), []byte("foreign"), 0o600); err != nil {
		t.Fatalf("write foreign owner.lock: %v", err)
	}
	before = snapshotTree(t, foreignFile)
	if code := runManagedState([]string{"hold", "--dir", foreignFile, "--operation", "owner"}, io.Discard, io.Discard, make(chan os.Signal, 2), constantPpid(1), time.Millisecond); code != 4 {
		t.Fatalf("foreign owner.lock: exit = %d, want 4", code)
	}
	if after := snapshotTree(t, foreignFile); !reflect.DeepEqual(before, after) {
		t.Fatalf("foreign-object refusal changed the filesystem: before=%v after=%v", before, after)
	}

	// A partial owner.lock directory (no complete record) is retained evidence
	// and fails closed (4) without being reclaimed.
	partial := managedStateRoot(t)
	if err := os.Mkdir(filepath.Join(partial, "owner.lock"), 0o700); err != nil {
		t.Fatalf("mkdir partial owner.lock: %v", err)
	}
	before = snapshotTree(t, partial)
	if code := runManagedState([]string{"hold", "--dir", partial, "--operation", "owner"}, io.Discard, io.Discard, make(chan os.Signal, 2), constantPpid(1), time.Millisecond); code != 4 {
		t.Fatalf("partial owner.lock: exit = %d, want 4", code)
	}
	if after := snapshotTree(t, partial); !reflect.DeepEqual(before, after) {
		t.Fatalf("retained-evidence refusal changed the filesystem: before=%v after=%v", before, after)
	}
}

func TestManagedStateHoldOwnerReadinessAndRelease(t *testing.T) {
	dir := managedStateRoot(t)
	reader, writer := io.Pipe()
	signals := make(chan os.Signal, 2)
	done := make(chan int, 1)
	go func() {
		done <- runManagedState([]string{"hold", "--dir", dir, "--operation", "owner"}, writer, io.Discard, signals, constantPpid(11), time.Millisecond)
	}()

	line := readLineWithin(t, reader, 5*time.Second)
	if !strings.Contains(line, `"ok":true`) || !strings.Contains(line, `"operation":"owner"`) {
		t.Fatalf("readiness = %q, want owner readiness JSON", strings.TrimSpace(line))
	}

	lockDir := filepath.Join(dir, "owner.lock")
	if info, err := os.Lstat(lockDir); err != nil || !info.IsDir() {
		t.Fatalf("owner.lock not held while holder runs: info=%v err=%v", info, err)
	}
	if _, err := os.Lstat(filepath.Join(lockDir, "owner.json")); err != nil {
		t.Fatalf("owner.json missing while held: %v", err)
	}

	signals <- syscall.SIGTERM
	if code := waitManagedState(t, done, 5*time.Second); code != 0 {
		t.Fatalf("SIGTERM exit = %d, want 0", code)
	}
	if _, err := os.Lstat(lockDir); !os.IsNotExist(err) {
		t.Fatalf("owner.lock retained after clean release: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "generation")); err != nil {
		t.Fatalf("generation file missing after clean release: %v", err)
	}
}

func TestManagedStateHoldOwnerRetainsOnInterrupt(t *testing.T) {
	dir := managedStateRoot(t)
	reader, writer := io.Pipe()
	signals := make(chan os.Signal, 2)
	done := make(chan int, 1)
	go func() {
		done <- runManagedState([]string{"hold", "--dir", dir, "--operation", "owner"}, writer, io.Discard, signals, constantPpid(12), time.Millisecond)
	}()

	readLineWithin(t, reader, 5*time.Second)

	signals <- syscall.SIGINT
	if code := waitManagedState(t, done, 5*time.Second); code != 5 {
		t.Fatalf("SIGINT exit = %d, want 5", code)
	}
	lockDir := filepath.Join(dir, "owner.lock")
	if info, err := os.Lstat(lockDir); err != nil || !info.IsDir() {
		t.Fatalf("owner.lock not retained after interrupt: info=%v err=%v", info, err)
	}
	if _, err := os.Lstat(filepath.Join(lockDir, "owner.json")); err != nil {
		t.Fatalf("owner.json not retained after interrupt: %v", err)
	}
}

func TestManagedStateHoldTransactionReadinessAndRelease(t *testing.T) {
	dir := managedStateRoot(t)
	reader, writer := io.Pipe()
	signals := make(chan os.Signal, 2)
	done := make(chan int, 1)
	go func() {
		done <- runManagedState([]string{"hold", "--dir", dir, "--operation", "transaction", "--timeout", "0"}, writer, io.Discard, signals, constantPpid(13), time.Millisecond)
	}()

	line := readLineWithin(t, reader, 5*time.Second)
	if !strings.Contains(line, `"ok":true`) || !strings.Contains(line, `"operation":"transaction"`) {
		t.Fatalf("readiness = %q, want transaction readiness JSON", strings.TrimSpace(line))
	}

	txnLock := filepath.Join(dir, "txn.lock")
	if info, err := os.Lstat(txnLock); err != nil || !info.IsDir() {
		t.Fatalf("txn.lock not held while holder runs: info=%v err=%v", info, err)
	}

	signals <- syscall.SIGTERM
	if code := waitManagedState(t, done, 5*time.Second); code != 0 {
		t.Fatalf("SIGTERM exit = %d, want 0", code)
	}
	if _, err := os.Lstat(txnLock); !os.IsNotExist(err) {
		t.Fatalf("txn.lock retained after clean release: %v", err)
	}
}

func TestManagedStateHoldParentDeathRetains(t *testing.T) {
	dir := managedStateRoot(t)
	signals := make(chan os.Signal, 2)
	var calls atomic.Int64
	ppid := func() int {
		if calls.Add(1) == 1 {
			return 100
		}
		return 101
	}
	done := make(chan int, 1)
	go func() {
		done <- runManagedState([]string{"hold", "--dir", dir, "--operation", "owner"}, io.Discard, io.Discard, signals, ppid, time.Millisecond)
	}()

	if code := waitManagedState(t, done, 5*time.Second); code != 5 {
		t.Fatalf("parent-death exit = %d, want 5", code)
	}
	lockDir := filepath.Join(dir, "owner.lock")
	if info, err := os.Lstat(lockDir); err != nil || !info.IsDir() {
		t.Fatalf("owner.lock not retained after parent death: info=%v err=%v", info, err)
	}
	if _, err := os.Lstat(filepath.Join(lockDir, "owner.json")); err != nil {
		t.Fatalf("owner.json not retained after parent death: %v", err)
	}
}

func TestManagedStateHoldTransactionTimeout(t *testing.T) {
	dir := managedStateRoot(t)
	txnLock := filepath.Join(dir, "txn.lock")
	if err := os.Mkdir(txnLock, 0o700); err != nil {
		t.Fatalf("mkdir foreign txn.lock: %v", err)
	}
	before := snapshotTree(t, dir)

	code := runManagedState(
		[]string{"hold", "--dir", dir, "--operation", "transaction", "--timeout", "50ms"},
		io.Discard, io.Discard, make(chan os.Signal, 2), constantPpid(14), time.Millisecond,
	)
	if code != 3 {
		t.Fatalf("transaction timeout exit = %d, want 3", code)
	}
	if after := snapshotTree(t, dir); !reflect.DeepEqual(before, after) {
		t.Fatalf("timeout refusal changed the filesystem: before=%v after=%v", before, after)
	}
}
