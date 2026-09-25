//go:build linux || darwin

package deviceauth

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
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

func TestOpenCreatesMissingNestedLockParentAndReopensPersistedStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runtime", "config", "plugin", "device-auth")
	parent := filepath.Dir(dir)
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("open with missing nested parent: %v", err)
	}
	if _, err := store.CreateInvitation("phone", RoleController, "en"); err != nil {
		t.Fatalf("persist invitation: %v", err)
	}
	parentInfo, err := os.Lstat(parent)
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode().Perm() != 0o700 {
		t.Fatalf("created lock parent = %v, err=%v; want protected 0700 directory", parentInfo, err)
	}
	lockPath := filepath.Join(parent, storeWriterLockName)
	lockBefore, err := os.Lstat(lockPath)
	if err != nil || lockBefore.Mode().Perm() != 0o600 {
		t.Fatalf("created writer lock = %v, err=%v; want mode 0600", lockBefore, err)
	}
	before, err := os.ReadFile(filepath.Join(dir, storeFilename))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err != nil {
		t.Fatalf("re-open persisted store: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(dir, storeFilename))
	if err != nil || !bytes.Equal(after, before) {
		t.Fatalf("re-opening persisted store changed bytes: equal=%t err=%v", bytes.Equal(after, before), err)
	}
	lockAfter, err := os.Lstat(lockPath)
	if err != nil || !os.SameFile(lockBefore, lockAfter) {
		t.Fatalf("re-open changed writer lock identity: before=%v after=%v err=%v", lockBefore, lockAfter, err)
	}
}

func TestOpenDeferredLeavesMissingParentAbsent(t *testing.T) {
	root := t.TempDir()
	runtimeDir := filepath.Join(root, "runtime")
	configDir := filepath.Join(runtimeDir, "config")
	parent := filepath.Join(configDir, "plugin")
	dir := filepath.Join(parent, "device-auth")
	if _, err := OpenDeferred(dir); err != nil {
		t.Fatalf("open deferred store: %v", err)
	}
	for _, path := range []string{runtimeDir, configDir, parent, dir, filepath.Join(parent, storeWriterLockName)} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("OpenDeferred created %q: lstat error = %v", path, err)
		}
	}
}

func TestOpenRefusesSymlinkOrWritableLockParent(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "target")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		parent := filepath.Join(root, "link")
		if err := os.Symlink(target, parent); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(filepath.Join(parent, "device-auth")); err == nil {
			t.Fatal("Open accepted a symlink lock parent")
		}
		if _, err := os.Lstat(filepath.Join(target, storeWriterLockName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("symlink target received a writer lock: %v", err)
		}
	})

	t.Run("writable", func(t *testing.T) {
		parent := t.TempDir()
		if err := os.Chmod(parent, 0o770); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(filepath.Join(parent, "device-auth")); err == nil {
			t.Fatal("Open accepted a group-writable lock parent")
		}
		info, err := os.Lstat(parent)
		if err != nil || info.Mode().Perm() != 0o770 {
			t.Fatalf("writable parent was modified: mode=%v err=%v", info, err)
		}
		if _, err := os.Lstat(filepath.Join(parent, storeWriterLockName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("writable parent received a writer lock: %v", err)
		}
	})
}

type writerLockTestFileInfo struct {
	os.FileInfo
	uid uint32
}

func (info writerLockTestFileInfo) Sys() any {
	return &syscall.Stat_t{Uid: info.uid}
}

func TestStoreWriterLockParentRefusesForeignOwner(t *testing.T) {
	info, err := os.Lstat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	foreignUID := uint32(os.Getuid()) + 1
	if foreignUID == uint32(os.Getuid()) {
		foreignUID++
	}
	if err := validateStoreWriterLockParent(writerLockTestFileInfo{FileInfo: info, uid: foreignUID}); err == nil {
		t.Fatal("accepted a lock parent owned by another user")
	}
}
