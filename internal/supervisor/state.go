package supervisor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	stateSchema             = 1
	maxSupervisorStateBytes = 64 * 1024
)

type Status string

const (
	StatusStarting Status = "starting"
	StatusRunning  Status = "running"
	StatusRetrying Status = "retrying"
	StatusTripped  Status = "tripped"
	StatusStopped  Status = "stopped"
	StatusReset    Status = "reset"
)

type State struct {
	Schema    int    `json:"schema"`
	Status    Status `json:"status"`
	Failures  int    `json:"failures"`
	Reason    string `json:"reason,omitempty"`
	UpdatedAt string `json:"updated_at"`
}

type stateFile interface {
	Name() string
	Chmod(os.FileMode) error
	Write([]byte) (int, error)
	Sync() error
	Close() error
}

type stateReadFile interface {
	io.Reader
	Stat() (os.FileInfo, error)
	Close() error
}

type stateDirectory interface {
	Sync() error
	Close() error
}

type stateIO struct {
	lstat         func(string) (os.FileInfo, error)
	open          func(string) (stateReadFile, error)
	mkdirAll      func(string, os.FileMode) error
	chmod         func(string, os.FileMode) error
	marshal       func(State) ([]byte, error)
	createTemp    func(string, string) (stateFile, error)
	rename        func(string, string) error
	openDirectory func(string) (stateDirectory, error)
	remove        func(string) error
	now           func() time.Time
}

func defaultStateIO() stateIO {
	return stateIO{
		lstat: os.Lstat, mkdirAll: os.MkdirAll, chmod: os.Chmod,
		open: func(path string) (stateReadFile, error) {
			return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		},
		marshal:    func(state State) ([]byte, error) { return json.MarshalIndent(state, "", "  ") },
		createTemp: func(directory, pattern string) (stateFile, error) { return os.CreateTemp(directory, pattern) },
		rename:     os.Rename, remove: os.Remove, now: time.Now,
		openDirectory: func(directory string) (stateDirectory, error) { return os.Open(directory) },
	}
}

func ReadState(path string) (State, error) {
	return readStateWith(defaultStateIO(), path)
}

func readStateWith(ops stateIO, path string) (State, error) {
	before, err := ops.lstat(path)
	if err != nil {
		return State{}, err
	}
	if !validStateFile(before) {
		return State{}, errors.New("supervisor state must be a private bounded regular file with one link")
	}
	file, err := ops.open(path)
	if err != nil {
		return State{}, err
	}
	after, statErr := file.Stat()
	if statErr != nil {
		_ = file.Close()
		return State{}, statErr
	}
	if !validStateFile(after) || !os.SameFile(before, after) {
		_ = file.Close()
		return State{}, errors.New("supervisor state changed or became unsafe while opening")
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxSupervisorStateBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return State{}, readErr
	}
	if closeErr != nil {
		return State{}, closeErr
	}
	if len(data) > maxSupervisorStateBytes {
		return State{}, errors.New("supervisor state exceeds size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var state State
	if err := decoder.Decode(&state); err != nil {
		return State{}, fmt.Errorf("decode supervisor state: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return State{}, errors.New("supervisor state has trailing content")
	}
	if state.Schema != stateSchema || !validStatus(state.Status) || state.Failures < 0 || len(state.Reason) > 500 || strings.TrimSpace(state.UpdatedAt) == "" {
		return State{}, errors.New("supervisor state has an unsupported format")
	}
	return state, nil
}

func validStateFile(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return info.Mode().IsRegular() && info.Mode().Perm()&0o077 == 0 && info.Size() <= maxSupervisorStateBytes && ok && stat.Nlink == 1
}

func Reset(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("supervisor state path must be absolute")
	}
	lock, err := acquireLifetimeLock(path + ".lock")
	if err != nil {
		return err
	}
	defer lock.Close()

	state, err := ReadState(path)
	if err != nil {
		return err
	}
	if state.Status != StatusTripped {
		return errors.New("supervisor is not tripped")
	}
	return writeState(path, State{Status: StatusReset})
}

func RecordBootstrapFailure(path string, maxFailures int, reason string) (State, error) {
	reason = safeReason(reason)
	if !filepath.IsAbs(path) || maxFailures < 1 || reason == "" {
		return State{}, errors.New("absolute supervisor state path, positive failure cap, and reason are required")
	}
	lock, err := acquireLifetimeLock(path + ".lock")
	if err != nil {
		return State{}, err
	}
	defer lock.Close()

	previous, readErr := ReadState(path)
	if readErr == nil && previous.Status == StatusTripped {
		return previous, nil
	}
	failures := 0
	if readErr == nil {
		failures = previous.Failures
	} else if !errors.Is(readErr, os.ErrNotExist) {
		failures = maxFailures - 1
		reason = safeReason("invalid prior supervisor state; " + reason)
	}
	failures++
	if failures > maxFailures {
		failures = maxFailures
	}
	status := StatusRetrying
	if failures >= maxFailures {
		status = StatusTripped
	}
	if err := writeState(path, State{Status: status, Failures: failures, Reason: reason}); err != nil {
		return State{}, err
	}
	return ReadState(path)
}

func validStatus(status Status) bool {
	switch status {
	case StatusStarting, StatusRunning, StatusRetrying, StatusTripped, StatusStopped, StatusReset:
		return true
	default:
		return false
	}
}

func writeState(path string, state State) error {
	return writeStateWith(defaultStateIO(), path, state)
}

func writeStateWith(ops stateIO, path string, state State) error {
	state.Schema = stateSchema
	state.Reason = safeReason(state.Reason)
	state.UpdatedAt = ops.now().UTC().Format(time.RFC3339Nano)
	data, err := ops.marshal(state)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	directory := filepath.Dir(path)
	if err := ops.mkdirAll(directory, 0o700); err != nil {
		return err
	}
	if err := ops.chmod(directory, 0o700); err != nil {
		return err
	}
	temporary, err := ops.createTemp(directory, ".supervisor-state.*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer ops.remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	written, err := temporary.Write(data)
	if err != nil {
		temporary.Close()
		return err
	}
	if written != len(data) {
		temporary.Close()
		return io.ErrShortWrite
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := ops.rename(temporaryPath, path); err != nil {
		return err
	}
	directoryHandle, err := ops.openDirectory(directory)
	if err != nil {
		return err
	}
	err = directoryHandle.Sync()
	closeErr := directoryHandle.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func safeReason(reason string) string {
	reason = strings.Join(strings.Fields(reason), " ")
	if len(reason) > 500 {
		reason = reason[:500]
		for !utf8.ValidString(reason) {
			reason = reason[:len(reason)-1]
		}
	}
	return reason
}
