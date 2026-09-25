//go:build linux || darwin

package deviceauth

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestStoreWriterLockIsExclusiveAndNonBlocking(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "device-auth")
	first, err := acquireStoreWriterLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := acquireStoreWriterLock(dir)
	if !errors.Is(err, ErrStoreWriterBusy) {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("second cooperating writer lock = %v, want immediate busy", err)
	}
}

func TestCooperatingConcurrentStoreWritersDoNotOverwrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "device-auth")
	if _, err := Open(dir); err != nil {
		t.Fatal(err)
	}
	first, err := OpenDeferred(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := OpenDeferred(dir)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for index, store := range []*Store{first, second} {
		wg.Add(1)
		go func(index int, store *Store) {
			defer wg.Done()
			<-start
			results <- store.ArmBootstrapInvitationTransactional(bytes.Repeat([]byte{byte(index + 1)}, secretBytes), "relay", "en")
		}(index, store)
	}
	close(start)
	wg.Wait()
	close(results)
	successes := 0
	failures := 0
	for err := range results {
		if err == nil {
			successes++
		} else {
			failures++
		}
	}
	if successes != 1 || failures != 1 {
		t.Fatalf("cooperating concurrent store writes = %d success, %d failure; want exactly one of each", successes, failures)
	}
	if _, err := Open(dir); err != nil {
		t.Fatalf("committed store is not readable after concurrent writes: %v", err)
	}
}

func TestOrdinaryPersistenceRejectsStaleCooperatingStoreWriter(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "device-auth")
	if _, err := Open(dir); err != nil {
		t.Fatal(err)
	}
	first, err := OpenDeferred(dir)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := OpenDeferred(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.ResetWithBootstrap(bytes.Repeat([]byte{1}, secretBytes), "first", "en"); err != nil {
		t.Fatalf("first ordinary writer: %v", err)
	}
	before, err := os.ReadFile(filepath.Join(dir, storeFilename))
	if err != nil {
		t.Fatal(err)
	}
	if err := stale.ResetWithBootstrap(bytes.Repeat([]byte{2}, secretBytes), "stale", "en"); err == nil {
		t.Fatal("stale ordinary Store instance overwrote a newer committed reset")
	}
	after, err := os.ReadFile(filepath.Join(dir, storeFilename))
	if err != nil || !bytes.Equal(after, before) {
		t.Fatalf("stale ordinary write changed committed bytes: equal=%t err=%v", bytes.Equal(after, before), err)
	}
}

func TestStoreWriterLockRefusesForeignLockfileWithoutChangingIt(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, storeWriterLockName)
	foreign := []byte("foreign lock metadata")
	if err := os.WriteFile(path, foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := acquireStoreWriterLock(filepath.Join(root, "device-auth")); err == nil {
		t.Fatal("accepted a foreign-mode writer lock file")
	}
	got, err := os.ReadFile(path)
	info, statErr := os.Stat(path)
	if err != nil || statErr != nil || !bytes.Equal(got, foreign) || info.Mode().Perm() != 0o640 {
		t.Fatalf("foreign lock file changed: bytes=%q mode=%v errors=%v/%v", got, info, err, statErr)
	}
}

func TestOpenExistingStoreDoesNotRewriteItsBytes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "device-auth")
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, storeFilename))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(dir, storeFilename))
	if err != nil || !bytes.Equal(after, before) {
		t.Fatalf("opening a valid store rewrote its bytes: equal=%t err=%v", bytes.Equal(after, before), err)
	}
	_ = store
}
