package deviceauth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const storeWriterLockName = ".herdr-deviceauth-writer.lock"

var ErrStoreWriterBusy = errors.New("device store writer is busy")

type storeWriterLock struct {
	file *os.File
}

// acquireStoreWriterLock reserves a stable lockfile in the existing runtime
// parent. The file remains in place after unlock so cooperating processes
// always lock the same inode; it is created only when a write is authorized.
func acquireStoreWriterLock(storeDir string) (*storeWriterLock, error) {
	if !storeWriterLockSupported() {
		return nil, errors.New("cooperating device-store writer locks are unsupported on this platform")
	}
	parent := filepath.Dir(filepath.Clean(storeDir))
	parentInfo, err := os.Lstat(parent)
	if err != nil {
		return nil, fmt.Errorf("inspect device-store lock parent: %w", err)
	}
	if parentInfo.Mode()&os.ModeSymlink != 0 || !parentInfo.IsDir() || parentInfo.Mode().Perm()&0o022 != 0 {
		return nil, errors.New("device-store lock parent is not a protected directory")
	}
	if uid, ok := fileOwner(parentInfo); !ok || uid != uint32(os.Getuid()) {
		return nil, errors.New("device-store lock parent is not owned by the current user")
	}

	path := filepath.Join(parent, storeWriterLockName)
	file, err := openStoreWriterLockFile(path)
	if err != nil {
		return nil, fmt.Errorf("open device-store writer lock: %w", err)
	}
	fail := func(err error) (*storeWriterLock, error) {
		_ = file.Close()
		return nil, err
	}
	if err := validateStoreWriterLockFile(path, file); err != nil {
		return fail(err)
	}
	currentParent, err := os.Lstat(parent)
	if err != nil || !os.SameFile(parentInfo, currentParent) {
		return fail(errors.New("device-store lock parent changed while opening writer lock"))
	}
	if err := lockStoreWriterFile(file); err != nil {
		if errors.Is(err, ErrStoreWriterBusy) {
			return fail(err)
		}
		return fail(fmt.Errorf("lock device-store writer: %w", err))
	}
	if err := validateStoreWriterLockFile(path, file); err != nil {
		_ = unlockStoreWriterFile(file)
		return fail(err)
	}
	currentParent, err = os.Lstat(parent)
	if err != nil || !os.SameFile(parentInfo, currentParent) {
		_ = unlockStoreWriterFile(file)
		return fail(errors.New("device-store lock parent changed while acquiring writer lock"))
	}
	return &storeWriterLock{file: file}, nil
}

func validateStoreWriterLockFile(path string, file *os.File) error {
	opened, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect open device-store writer lock: %w", err)
	}
	if !opened.Mode().IsRegular() || opened.Mode().Perm() != 0o600 || fileNlink(opened) != 1 {
		return errors.New("device-store writer lock is not a private singly-linked regular file")
	}
	if uid, ok := fileOwner(opened); !ok || uid != uint32(os.Getuid()) {
		return errors.New("device-store writer lock is not owned by the current user")
	}
	current, err := os.Lstat(path)
	if err != nil || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, current) {
		return errors.New("device-store writer lock path was replaced")
	}
	return nil
}

func (l *storeWriterLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := unlockStoreWriterFile(l.file)
	closeErr := l.file.Close()
	l.file = nil
	return errors.Join(unlockErr, closeErr)
}
