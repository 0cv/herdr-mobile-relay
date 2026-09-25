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

// prepareStoreWriterLockParent is used only by ordinary Open. Legacy Open
// protected the store with MkdirAll, so it also created missing parent
// components. Deferred managed opens deliberately do not call this helper.
func prepareStoreWriterLockParent(storeDir string) error {
	if !storeWriterLockSupported() {
		return errors.New("cooperating device-store writer locks are unsupported on this platform")
	}
	parent := filepath.Dir(filepath.Clean(storeDir))
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create device-store lock parent: %w", err)
	}
	parentInfo, err := os.Lstat(parent)
	if err != nil {
		return fmt.Errorf("inspect device-store lock parent: %w", err)
	}
	return validateStoreWriterLockParent(parentInfo)
}

func validateStoreWriterLockParent(info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o022 != 0 {
		return errors.New("device-store lock parent is not a protected directory")
	}
	if uid, ok := fileOwner(info); !ok || uid != uint32(os.Getuid()) {
		return errors.New("device-store lock parent is not owned by the current user")
	}
	return nil
}

// acquireStoreWriterLock reserves a stable lockfile in the runtime parent. The
// file remains in place after unlock so cooperating processes always lock the
// same inode; it is created only when a write is authorized.
func acquireStoreWriterLock(storeDir string) (*storeWriterLock, error) {
	if !storeWriterLockSupported() {
		return nil, errors.New("cooperating device-store writer locks are unsupported on this platform")
	}
	parent := filepath.Dir(filepath.Clean(storeDir))
	parentInfo, err := os.Lstat(parent)
	if err != nil {
		return nil, fmt.Errorf("inspect device-store lock parent: %w", err)
	}
	if err := validateStoreWriterLockParent(parentInfo); err != nil {
		return nil, err
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
