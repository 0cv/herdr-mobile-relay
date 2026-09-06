package activeruntime

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSharedLeaseBlocksPromotionUntilEffectReleasesIt(t *testing.T) {
	activePath := filepath.Join(t.TempDir(), "active-runtime.json")
	shared, err := AcquireSharedLease(activePath)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	acquired := make(chan error, 1)
	go func() {
		close(started)
		exclusive, acquireErr := acquireLease(activePath, syscall.LOCK_EX)
		if acquireErr == nil {
			acquireErr = exclusive.Close()
		}
		acquired <- acquireErr
	}()
	<-started
	select {
	case err := <-acquired:
		t.Fatalf("promotion acquired while effect lease was held: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := shared.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("promotion did not acquire after effect lease was released")
	}
	info, err := os.Lstat(filepath.Join(filepath.Dir(activePath), LeaseName))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("lease file = (%v, %v), want owned 0600 file", info, err)
	}
}

func TestSharedLeaseRejectsUnsafeFileAndPath(t *testing.T) {
	root := t.TempDir()
	activePath := filepath.Join(root, "active-runtime.json")
	lockPath := filepath.Join(root, LeaseName)
	if err := os.WriteFile(lockPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if lease, err := AcquireSharedLease(activePath); err == nil || lease != nil || !strings.Contains(err.Error(), "owned regular 0600") {
		t.Fatalf("unsafe lease file = (%v, %v)", lease, err)
	}
	if lease, err := AcquireSharedLease("relative/active-runtime.json"); err == nil || lease != nil {
		t.Fatalf("relative lease path = (%v, %v)", lease, err)
	}
}

func TestSharedLeaseCoversEveryAcquisitionBoundary(t *testing.T) {
	root := t.TempDir()
	activePath := filepath.Join(root, "active-runtime.json")
	lockPath := filepath.Join(root, LeaseName)
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(lockPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	base := func() leaseIO {
		return leaseIO{
			openFile: func(string, int, os.FileMode) (*os.File, error) { return file, nil },
			stat:     func(*os.File) (os.FileInfo, error) { return info, nil },
			flock:    func(int, int) error { return nil },
			lstat:    func(string) (os.FileInfo, error) { return info, nil },
			sameFile: func(os.FileInfo, os.FileInfo) bool { return true },
			close:    func(*os.File) error { return nil },
		}
	}
	for _, test := range []struct {
		name   string
		mutate func(*leaseIO)
	}{
		{name: "open", mutate: func(ops *leaseIO) {
			ops.openFile = func(string, int, os.FileMode) (*os.File, error) { return nil, errors.New("open") }
		}},
		{name: "stat", mutate: func(ops *leaseIO) { ops.stat = func(*os.File) (os.FileInfo, error) { return nil, errors.New("stat") } }},
		{name: "flock", mutate: func(ops *leaseIO) { ops.flock = func(int, int) error { return errors.New("flock") } }},
		{name: "revalidate", mutate: func(ops *leaseIO) { ops.lstat = func(string) (os.FileInfo, error) { return nil, errors.New("lstat") } }},
		{name: "changed", mutate: func(ops *leaseIO) { ops.sameFile = func(os.FileInfo, os.FileInfo) bool { return false } }},
		{name: "unsafe current", mutate: func(ops *leaseIO) {
			ops.lstat = func(string) (os.FileInfo, error) { return transactionDirectoryInfo(t), nil }
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ops := base()
			test.mutate(&ops)
			if lease, err := acquireLeaseWith(ops, activePath, syscall.LOCK_SH); err == nil || lease != nil {
				t.Fatalf("acquire result = (%v, %v)", lease, err)
			}
		})
	}

	interrupts := 0
	ops := base()
	ops.flock = func(_ int, operation int) error {
		if operation != syscall.LOCK_UN && interrupts == 0 {
			interrupts++
			return syscall.EINTR
		}
		return nil
	}
	lease, err := acquireLeaseWith(ops, activePath, syscall.LOCK_SH)
	if err != nil {
		t.Fatal(err)
	}
	if interrupts != 1 {
		t.Fatalf("flock interrupts = %d", interrupts)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSharedLeaseCloseIsNilSafeIdempotentAndPreservesErrors(t *testing.T) {
	if err := (*Lease)(nil).Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "lease")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	lease := &Lease{
		file: file,
		flock: func(int, int) error {
			calls++
			return errors.New("unlock")
		},
		closeFile: func(*os.File) error { return errors.New("close") },
	}
	if err := lease.Close(); err == nil {
		t.Fatal("lease close errors were ignored")
	}
	if err := lease.Close(); err == nil || calls != 1 {
		t.Fatalf("idempotent close = (%v, calls=%d)", err, calls)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
