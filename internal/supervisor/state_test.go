package supervisor

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadStateRejectsEveryUnsafeOrInvalidRepresentation(t *testing.T) {
	valid := `{"schema":1,"status":"running","failures":0,"updated_at":"2026-09-06T00:00:00Z"}`
	t.Run("missing", func(t *testing.T) {
		if _, err := ReadState(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("missing error = %v", err)
		}
	})
	t.Run("directory", func(t *testing.T) {
		if _, err := ReadState(t.TempDir()); err == nil {
			t.Fatal("directory accepted")
		}
	})
	t.Run("public", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state")
		writeStateBytes(t, path, valid, 0o644)
		if _, err := ReadState(path); err == nil {
			t.Fatal("public state accepted")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "target")
		writeStateBytes(t, target, valid, 0o600)
		path := filepath.Join(root, "state")
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadState(path); err == nil {
			t.Fatal("symlink state accepted")
		}
	})
	t.Run("hardlink", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "state")
		writeStateBytes(t, path, valid, 0o600)
		if err := os.Link(path, filepath.Join(root, "second-link")); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadState(path); err == nil {
			t.Fatal("hardlinked state accepted")
		}
	})
	t.Run("same-inode symlink swap", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "state")
		writeStateBytes(t, path, valid, 0o600)
		moved := filepath.Join(root, "moved")
		ops := defaultStateIO()
		ops.lstat = func(string) (os.FileInfo, error) {
			info, err := os.Lstat(path)
			if err != nil {
				return nil, err
			}
			if err := os.Rename(path, moved); err != nil {
				return nil, err
			}
			if err := os.Symlink(moved, path); err != nil {
				return nil, err
			}
			return info, nil
		}
		if _, err := readStateWith(ops, path); err == nil {
			t.Fatal("same-inode symlink swap accepted")
		}
	})
	t.Run("oversized", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state")
		writeStateBytes(t, path, strings.Repeat("x", 64*1024+1), 0o600)
		if _, err := ReadState(path); err == nil {
			t.Fatal("oversized state accepted")
		}
	})

	invalid := map[string]string{
		"malformed":        `{`,
		"trailing value":   valid + `{}`,
		"trailing invalid": valid + `nope`,
		"unknown field":    strings.TrimSuffix(valid, `}`) + `,"future":true}`,
		"schema":           strings.Replace(valid, `"schema":1`, `"schema":2`, 1),
		"status":           strings.Replace(valid, `"status":"running"`, `"status":"future"`, 1),
		"failures":         strings.Replace(valid, `"failures":0`, `"failures":-1`, 1),
		"reason":           strings.TrimSuffix(valid, `}`) + `,"reason":"` + strings.Repeat("x", 501) + `"}`,
		"updated at":       strings.Replace(valid, `"updated_at":"2026-09-06T00:00:00Z"`, `"updated_at":" "`, 1),
	}
	for name, content := range invalid {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state")
			writeStateBytes(t, path, content, 0o600)
			if _, err := ReadState(path); err == nil {
				t.Fatalf("invalid state accepted: %s", content)
			}
		})
	}
}

func TestReadStateRejectsEveryOpenReadBoundaryFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	valid := `{"schema":1,"status":"running","failures":0,"updated_at":"2026-09-06T00:00:00Z"}`
	writeStateBytes(t, path, valid, 0o600)
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(filepath.Dir(path), "other")
	writeStateBytes(t, other, valid, 0o600)
	otherInfo, err := os.Lstat(other)
	if err != nil {
		t.Fatal(err)
	}
	directoryInfo, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]func(*stateIO){
		"open": func(ops *stateIO) {
			ops.open = func(string) (stateReadFile, error) { return nil, errors.New("open") }
		},
		"stat": func(ops *stateIO) {
			ops.open = func(string) (stateReadFile, error) { return &fixtureStateReadFile{statErr: errors.New("stat")}, nil }
		},
		"unsafe after open": func(ops *stateIO) {
			ops.open = func(string) (stateReadFile, error) { return &fixtureStateReadFile{info: directoryInfo}, nil }
		},
		"changed after open": func(ops *stateIO) {
			ops.open = func(string) (stateReadFile, error) { return &fixtureStateReadFile{info: otherInfo}, nil }
		},
		"read": func(ops *stateIO) {
			ops.open = func(string) (stateReadFile, error) {
				return &fixtureStateReadFile{info: info, readErr: errors.New("read")}, nil
			}
		},
		"close": func(ops *stateIO) {
			ops.open = func(string) (stateReadFile, error) {
				return &fixtureStateReadFile{info: info, reader: strings.NewReader(valid), closeErr: errors.New("close")}, nil
			}
		},
		"growth after stat": func(ops *stateIO) {
			ops.open = func(string) (stateReadFile, error) {
				return &fixtureStateReadFile{info: info, reader: strings.NewReader(strings.Repeat("x", maxSupervisorStateBytes+1))}, nil
			}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			ops := defaultStateIO()
			ops.lstat = func(string) (os.FileInfo, error) { return info, nil }
			mutate(&ops)
			if _, err := readStateWith(ops, path); err == nil {
				t.Fatal("boundary failure was accepted")
			}
		})
	}
}

func TestResetRequiresReadableTrippedState(t *testing.T) {
	if err := Reset("relative-state"); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative reset = %v", err)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if err := Reset(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing reset = %v", err)
	}
	path := filepath.Join(t.TempDir(), "state")
	writeStateBytes(t, path, `{"schema":1,"status":"running","failures":0,"updated_at":"2026-09-06T00:00:00Z"}`, 0o600)
	if err := Reset(path); err == nil || !strings.Contains(err.Error(), "not tripped") {
		t.Fatalf("running reset = %v", err)
	}
}

func TestEverySupervisorStatusIsExplicit(t *testing.T) {
	for _, status := range []Status{StatusStarting, StatusRunning, StatusRetrying, StatusTripped, StatusStopped, StatusReset} {
		if !validStatus(status) {
			t.Fatalf("valid status rejected: %s", status)
		}
	}
	if validStatus("future") {
		t.Fatal("unknown status accepted")
	}
}

func TestWriteStatePropagatesEveryAtomicBoundaryFailure(t *testing.T) {
	directory := t.TempDir()
	base := func() stateIO {
		return stateIO{
			mkdirAll: func(string, os.FileMode) error { return nil },
			chmod:    func(string, os.FileMode) error { return nil },
			marshal:  func(state State) ([]byte, error) { return json.Marshal(state) },
			createTemp: func(string, string) (stateFile, error) {
				return &failingStateFile{name: filepath.Join(directory, "temporary")}, nil
			},
			rename: func(string, string) error { return nil },
			openDirectory: func(string) (stateDirectory, error) {
				return failingStateDirectory{}, nil
			},
			remove: func(string) error { return nil },
			now:    func() time.Time { return time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC) },
		}
	}
	tests := map[string]func(*stateIO){
		"marshal": func(ops *stateIO) { ops.marshal = func(State) ([]byte, error) { return nil, errors.New("marshal") } },
		"mkdir":   func(ops *stateIO) { ops.mkdirAll = func(string, os.FileMode) error { return errors.New("mkdir") } },
		"chmod directory": func(ops *stateIO) {
			ops.chmod = func(string, os.FileMode) error { return errors.New("chmod directory") }
		},
		"create": func(ops *stateIO) {
			ops.createTemp = func(string, string) (stateFile, error) { return nil, errors.New("create") }
		},
		"chmod file": func(ops *stateIO) { ops.createTemp = failingStateTemp(directory, "chmod") },
		"write":      func(ops *stateIO) { ops.createTemp = failingStateTemp(directory, "write") },
		"short write": func(ops *stateIO) {
			ops.createTemp = failingStateTemp(directory, "short write")
		},
		"sync":   func(ops *stateIO) { ops.createTemp = failingStateTemp(directory, "sync") },
		"close":  func(ops *stateIO) { ops.createTemp = failingStateTemp(directory, "close") },
		"rename": func(ops *stateIO) { ops.rename = func(string, string) error { return errors.New("rename") } },
		"open directory": func(ops *stateIO) {
			ops.openDirectory = func(string) (stateDirectory, error) { return nil, errors.New("open directory") }
		},
		"sync directory": func(ops *stateIO) {
			ops.openDirectory = func(string) (stateDirectory, error) { return failingStateDirectory{fail: "sync"}, nil }
		},
		"close directory": func(ops *stateIO) {
			ops.openDirectory = func(string) (stateDirectory, error) { return failingStateDirectory{fail: "close"}, nil }
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			ops := base()
			mutate(&ops)
			if err := writeStateWith(ops, filepath.Join(directory, "state"), State{Status: StatusRunning}); err == nil {
				t.Fatal("atomic boundary failure was ignored")
			}
		})
	}
}

func TestWriteStateNormalizesAndUnicodeSafelyBoundsReason(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	reason := "  spaced\n" + strings.Repeat("é", 300)
	if err := writeState(path, State{Status: StatusRetrying, Failures: 2, Reason: reason}); err != nil {
		t.Fatal(err)
	}
	state, err := ReadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Reason) > 500 || !strings.HasPrefix(state.Reason, "spaced ") || strings.Contains(state.Reason, "\n") {
		t.Fatalf("normalized reason = %q (%d bytes)", state.Reason, len(state.Reason))
	}
}

func TestRecordBootstrapFailureTripsAtCapAndCanBeReset(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "supervisor.json")
	for failure := 1; failure <= 5; failure++ {
		state, err := RecordBootstrapFailure(statePath, 5, "invalid service configuration")
		if err != nil {
			t.Fatalf("failure %d: %v", failure, err)
		}
		wantStatus := StatusRetrying
		if failure == 5 {
			wantStatus = StatusTripped
		}
		if state.Status != wantStatus || state.Failures != failure || state.Reason != "invalid service configuration" {
			t.Fatalf("failure %d state = %+v", failure, state)
		}
	}
	state, err := RecordBootstrapFailure(statePath, 5, "must not advance a trip")
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != StatusTripped || state.Failures != 5 || state.Reason != "invalid service configuration" {
		t.Fatalf("persisted trip changed = %+v", state)
	}
	if err := Reset(statePath); err != nil {
		t.Fatal(err)
	}
	state, err = RecordBootstrapFailure(statePath, 5, "failed after reset")
	if err != nil || state.Status != StatusRetrying || state.Failures != 1 || state.Reason != "failed after reset" {
		t.Fatalf("failure after reset = %+v, %v", state, err)
	}
}

func TestRecordBootstrapFailureConvertsMalformedStateToVisibleTrip(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "supervisor.json")
	writeStateBytes(t, statePath, "not-json\n", 0o600)
	state, err := RecordBootstrapFailure(statePath, 5, "bootstrap validation failed")
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != StatusTripped || state.Failures != 5 || !strings.Contains(state.Reason, "invalid prior supervisor state") || !strings.Contains(state.Reason, "bootstrap validation failed") {
		t.Fatalf("malformed state recovery = %+v", state)
	}
	persisted, err := ReadState(statePath)
	if err != nil || persisted != state {
		t.Fatalf("persisted malformed-state trip = %+v, %v", persisted, err)
	}
}

func TestRecordBootstrapFailureCapsCorruptCountAndPropagatesWriteFailure(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "supervisor.json")
	if err := writeState(statePath, State{Status: StatusRetrying, Failures: 99, Reason: "old"}); err != nil {
		t.Fatal(err)
	}
	state, err := RecordBootstrapFailure(statePath, 5, "new")
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != StatusTripped || state.Failures != 5 {
		t.Fatalf("capped corrupt count = %+v", state)
	}

	directoryPath := filepath.Join(t.TempDir(), "state-directory")
	if err := os.Mkdir(directoryPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := RecordBootstrapFailure(directoryPath, 5, "failure"); err == nil {
		t.Fatal("bootstrap failure ignored an unwritable state target")
	}
}

func TestRecordBootstrapFailureRejectsInvalidInputAndConcurrentLifetime(t *testing.T) {
	root := t.TempDir()
	statePath := filepath.Join(root, "supervisor.json")
	tests := map[string]struct {
		path        string
		maxFailures int
		reason      string
	}{
		"relative path": {"supervisor.json", 5, "failure"},
		"zero cap":      {statePath, 0, "failure"},
		"blank reason":  {statePath, 5, " \t"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := RecordBootstrapFailure(test.path, test.maxFailures, test.reason); err == nil {
				t.Fatal("invalid bootstrap failure input accepted")
			}
		})
	}
	lock, err := acquireLifetimeLock(statePath + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := RecordBootstrapFailure(statePath, 5, "failure"); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("concurrent record error = %v, want ErrAlreadyRunning", err)
	}
	if err := writeState(statePath, State{Status: StatusTripped, Failures: 5}); err != nil {
		t.Fatal(err)
	}
	if err := Reset(statePath); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("concurrent reset error = %v, want ErrAlreadyRunning", err)
	}
}

type failingStateFile struct {
	name string
	fail string
}

type fixtureStateReadFile struct {
	reader   io.Reader
	info     os.FileInfo
	statErr  error
	readErr  error
	closeErr error
}

func (f *fixtureStateReadFile) Read(data []byte) (int, error) {
	if f.readErr != nil {
		return 0, f.readErr
	}
	if f.reader == nil {
		return 0, io.EOF
	}
	return f.reader.Read(data)
}

func (f *fixtureStateReadFile) Stat() (os.FileInfo, error) { return f.info, f.statErr }
func (f *fixtureStateReadFile) Close() error               { return f.closeErr }

func (f *failingStateFile) Name() string { return f.name }
func (f *failingStateFile) Chmod(os.FileMode) error {
	if f.fail == "chmod" {
		return errors.New("chmod")
	}
	return nil
}
func (f *failingStateFile) Write(data []byte) (int, error) {
	if f.fail == "write" {
		return 0, errors.New("write")
	}
	if f.fail == "short write" {
		return len(data) - 1, nil
	}
	return len(data), nil
}
func (f *failingStateFile) Sync() error {
	if f.fail == "sync" {
		return errors.New("sync")
	}
	return nil
}
func (f *failingStateFile) Close() error {
	if f.fail == "close" {
		return errors.New("close")
	}
	return nil
}

type failingStateDirectory struct{ fail string }

func (d failingStateDirectory) Sync() error {
	if d.fail == "sync" {
		return errors.New("sync")
	}
	return nil
}
func (d failingStateDirectory) Close() error {
	if d.fail == "close" {
		return errors.New("close")
	}
	return nil
}

func failingStateTemp(directory, fail string) func(string, string) (stateFile, error) {
	return func(string, string) (stateFile, error) {
		return &failingStateFile{name: filepath.Join(directory, "temporary"), fail: fail}, nil
	}
}

func writeStateBytes(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}
