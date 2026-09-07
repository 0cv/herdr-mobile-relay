package activeruntime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

const LeaseName = "active-runtime.lock"

// Lease holds the shared advisory lock that prevents Guardian from publishing
// a different active runtime while Relay revalidates and performs one effect.
type Lease struct {
	file      *os.File
	flock     func(int, int) error
	closeFile func(*os.File) error
	once      sync.Once
	err       error
}

type leaseIO struct {
	openFile func(string, int, os.FileMode) (*os.File, error)
	stat     func(*os.File) (os.FileInfo, error)
	flock    func(int, int) error
	lstat    func(string) (os.FileInfo, error)
	sameFile func(os.FileInfo, os.FileInfo) bool
	close    func(*os.File) error
}

func defaultLeaseIO() leaseIO {
	return leaseIO{
		openFile: os.OpenFile,
		stat:     func(file *os.File) (os.FileInfo, error) { return file.Stat() },
		flock:    syscall.Flock,
		lstat:    os.Lstat,
		sameFile: os.SameFile,
		close:    func(file *os.File) error { return file.Close() },
	}
}

// AcquireSharedLease takes a blocking shared flock on the private lock file
// beside active-runtime.json. Every publisher must take an exclusive flock on
// the same inode around its final revalidation and durable pointer publish.
func AcquireSharedLease(activeRuntimePath string) (*Lease, error) {
	return acquireLease(activeRuntimePath, syscall.LOCK_SH)
}

func acquireLease(activeRuntimePath string, operation int) (*Lease, error) {
	return acquireLeaseWith(defaultLeaseIO(), activeRuntimePath, operation)
}

func acquireLeaseWith(ops leaseIO, activeRuntimePath string, operation int) (*Lease, error) {
	if !normalizedAbsolute(activeRuntimePath) || filepath.Base(activeRuntimePath) != "active-runtime.json" {
		return nil, errors.New("active runtime lease requires an absolute normalized active-runtime.json path")
	}
	lockPath := filepath.Join(filepath.Dir(activeRuntimePath), LeaseName)
	file, err := ops.openFile(lockPath, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open active runtime lease: %w", err)
	}
	fail := func(err error) (*Lease, error) {
		_ = ops.close(file)
		return nil, err
	}
	info, err := ops.stat(file)
	if err != nil {
		return fail(fmt.Errorf("inspect active runtime lease: %w", err))
	}
	if err := validateLeaseFile(info); err != nil {
		return fail(err)
	}
	for {
		err = ops.flock(int(file.Fd()), operation)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		return fail(fmt.Errorf("lock active runtime lease: %w", err))
	}
	current, err := ops.lstat(lockPath)
	if err != nil || !ops.sameFile(info, current) {
		_ = ops.flock(int(file.Fd()), syscall.LOCK_UN)
		if err != nil {
			return fail(fmt.Errorf("revalidate active runtime lease: %w", err))
		}
		return fail(errors.New("active runtime lease changed while locking"))
	}
	if err := validateLeaseFile(current); err != nil {
		_ = ops.flock(int(file.Fd()), syscall.LOCK_UN)
		return fail(err)
	}
	return &Lease{file: file, flock: ops.flock, closeFile: ops.close}, nil
}

func validateLeaseFile(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !ok || stat.Nlink != 1 || stat.Uid != uint32(os.Getuid()) {
		return errors.New("active runtime lease must be an owned regular 0600 file with one link")
	}
	return nil
}

func (l *Lease) Close() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		unlockErr := l.flock(int(l.file.Fd()), syscall.LOCK_UN)
		l.err = errors.Join(unlockErr, l.closeFile(l.file))
	})
	return l.err
}
