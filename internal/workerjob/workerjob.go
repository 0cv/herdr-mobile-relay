// Package workerjob holds what the update and app deployment workers share: the job
// file they are started with, the lock that keeps one worker running, and the recovery
// of state left behind by a worker that died.
package workerjob

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

var errBusy = errors.New("lock is held")

// StartupGrace is how long a freshly scheduled worker may take to start before
// its state counts as abandoned.
const StartupGrace = 30 * time.Second

// Load reads the JSON job file at filename. what names the job in error messages.
func Load[T any](filename, what string) (T, error) {
	var job T
	data, err := os.ReadFile(filename)
	if err != nil {
		return job, fmt.Errorf("read %s job: %w", what, err)
	}
	if err := json.Unmarshal(data, &job); err != nil {
		return job, fmt.Errorf("parse %s job: %w", what, err)
	}
	return job, nil
}

// Lock takes the exclusive lock file at filename without waiting and writes the
// process ID into it. It returns busy when another process holds the lock.
func Lock(filename string, busy error) (*os.File, error) {
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, busy
		}
		return nil, fmt.Errorf("lock %s: %w", filename, err)
	}
	_ = file.Truncate(0)
	_, _ = fmt.Fprintf(file, "%d\n", os.Getpid())
	_ = file.Sync()
	return file, nil
}

// RecoverOrphan runs markFailed while holding the lock at lockPath, so a worker
// that is still running keeps its state. It does nothing when the lock is held.
func RecoverOrphan(lockPath string, markFailed func()) {
	lock, err := Lock(lockPath, errBusy)
	if err != nil {
		return
	}
	defer lock.Close()
	markFailed()
}
