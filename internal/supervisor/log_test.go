package supervisor

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewRotatingWriterRejectsLimitsAndFilesystemFailures(t *testing.T) {
	for _, limits := range [][2]int64{{0, 0}, {1, -1}} {
		if _, err := newRotatingWriter("/tmp/log", limits[0], int(limits[1])); err == nil {
			t.Fatalf("invalid limits accepted: %v", limits)
		}
	}
	base := defaultLogIO()
	base.mkdirAll = func(string, os.FileMode) error { return errors.New("mkdir") }
	if _, err := newRotatingWriterWith(base, filepath.Join(t.TempDir(), "log"), 1, 0); err == nil {
		t.Fatal("mkdir failure ignored")
	}
	base = defaultLogIO()
	base.chmod = func(string, os.FileMode) error { return errors.New("chmod") }
	if _, err := newRotatingWriterWith(base, filepath.Join(t.TempDir(), "log"), 1, 0); err == nil {
		t.Fatal("chmod failure ignored")
	}
	base = defaultLogIO()
	base.lstat = func(string) (os.FileInfo, error) { return nil, errors.New("lstat") }
	if _, err := newRotatingWriterWith(base, filepath.Join(t.TempDir(), "log"), 1, 0); err == nil || err.Error() != "lstat" {
		t.Fatalf("lstat failure = %v", err)
	}
	t.Run("symlink directory", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "target")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		linked := filepath.Join(root, "linked")
		if err := os.Symlink(target, linked); err != nil {
			t.Fatal(err)
		}
		if _, err := newRotatingWriter(filepath.Join(linked, "log"), 10, 1); err == nil {
			t.Fatal("symlinked log directory accepted")
		}
	})
}

func TestRotatingWriterPropagatesStatRotateAndFileFailures(t *testing.T) {
	base := func() logIO {
		return logIO{
			mkdirAll: func(string, os.FileMode) error { return nil },
			chmod:    func(string, os.FileMode) error { return nil },
			lstat: func(path string) (os.FileInfo, error) {
				if path == "/fixture" {
					return logFileInfo{mode: os.ModeDir | 0o700}, nil
				}
				return nil, os.ErrNotExist
			},
			openFile: func(string, int, os.FileMode) (logFile, error) { return &failingLogFile{}, nil },
			remove:   func(string) error { return nil },
			rename:   func(string, string) error { return nil },
		}
	}
	tests := map[string]func(*logIO){
		"stat": func(ops *logIO) {
			ops.lstat = func(path string) (os.FileInfo, error) {
				if path == "/fixture" {
					return logFileInfo{mode: os.ModeDir | 0o700}, nil
				}
				return nil, errors.New("stat")
			}
		},
		"rotate": func(ops *logIO) {
			ops.lstat = func(path string) (os.FileInfo, error) {
				if path == "/fixture" {
					return logFileInfo{mode: os.ModeDir | 0o700}, nil
				}
				return logFileInfo{size: 10, mode: 0o600}, nil
			}
			ops.rename = func(string, string) error { return errors.New("rotate") }
		},
		"open": func(ops *logIO) {
			ops.openFile = func(string, int, os.FileMode) (logFile, error) { return nil, errors.New("open") }
		},
		"chmod": func(ops *logIO) {
			ops.openFile = func(string, int, os.FileMode) (logFile, error) { return &failingLogFile{fail: "chmod"}, nil }
		},
		"write": func(ops *logIO) {
			ops.openFile = func(string, int, os.FileMode) (logFile, error) { return &failingLogFile{fail: "write"}, nil }
		},
		"short write": func(ops *logIO) {
			ops.openFile = func(string, int, os.FileMode) (logFile, error) { return &failingLogFile{fail: "short write"}, nil }
		},
		"opened stat": func(ops *logIO) {
			ops.openFile = func(string, int, os.FileMode) (logFile, error) { return &failingLogFile{fail: "stat"}, nil }
		},
		"opened non regular": func(ops *logIO) {
			ops.openFile = func(string, int, os.FileMode) (logFile, error) { return &failingLogFile{mode: os.ModeDir}, nil }
		},
		"close": func(ops *logIO) {
			ops.openFile = func(string, int, os.FileMode) (logFile, error) { return &failingLogFile{fail: "close"}, nil }
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			ops := base()
			mutate(&ops)
			writer, err := newRotatingWriterWith(ops, "/fixture/log", 10, 1)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Write([]byte("x")); err == nil {
				t.Fatal("writer failure ignored")
			}
		})
	}
}

func TestRotatingWriterRejectsSymlinkedLogFile(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "relay.log")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	writer, err := newRotatingWriter(path, 1024, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("secret")); err == nil {
		t.Fatal("symlinked log file accepted")
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "untouched" {
		t.Fatalf("symlink target changed to %q", content)
	}
}

func TestRotateCoversZeroMissingAndEveryBackupFailure(t *testing.T) {
	base := func(backups int) *rotatingWriter {
		return &rotatingWriter{
			path: "/fixture/log", maxBytes: 10, backups: backups,
			ops: logIO{remove: func(string) error { return nil }, rename: func(string, string) error { return nil }},
		}
	}
	if err := base(0).rotate(); err != nil {
		t.Fatalf("zero-backup rotate = %v", err)
	}
	missing := base(0)
	missing.ops.remove = func(string) error { return os.ErrNotExist }
	if err := missing.rotate(); err != nil {
		t.Fatalf("missing zero-backup rotate = %v", err)
	}
	removeFailure := base(2)
	removeFailure.ops.remove = func(string) error { return errors.New("remove") }
	if err := removeFailure.rotate(); err == nil {
		t.Fatal("backup remove failure ignored")
	}
	innerFailure := base(2)
	innerFailure.ops.rename = func(old, _ string) error {
		if old == "/fixture/log.1" {
			return errors.New("inner rename")
		}
		return nil
	}
	if err := innerFailure.rotate(); err == nil {
		t.Fatal("inner rename failure ignored")
	}
	missingInner := base(2)
	missingInner.ops.rename = func(old, _ string) error {
		if old == "/fixture/log.1" {
			return os.ErrNotExist
		}
		return nil
	}
	if err := missingInner.rotate(); err != nil {
		t.Fatalf("missing inner backup = %v", err)
	}
	finalFailure := base(1)
	finalFailure.ops.rename = func(string, string) error { return errors.New("final rename") }
	if err := finalFailure.rotate(); err == nil {
		t.Fatal("final rename failure ignored")
	}
}

type failingLogFile struct {
	fail string
	mode os.FileMode
}

func (f *failingLogFile) Chmod(os.FileMode) error {
	if f.fail == "chmod" {
		return errors.New("chmod")
	}
	return nil
}
func (f *failingLogFile) Write(data []byte) (int, error) {
	if f.fail == "write" {
		return 0, errors.New("write")
	}
	if f.fail == "short write" {
		return len(data) - 1, nil
	}
	return len(data), nil
}
func (f *failingLogFile) Stat() (os.FileInfo, error) {
	if f.fail == "stat" {
		return nil, errors.New("stat")
	}
	mode := f.mode
	if mode == 0 {
		mode = 0o600
	}
	return logFileInfo{mode: mode}, nil
}
func (f *failingLogFile) Close() error {
	if f.fail == "close" {
		return errors.New("close")
	}
	return nil
}

type logFileInfo struct {
	size int64
	mode os.FileMode
}

func (i logFileInfo) Name() string       { return "log" }
func (i logFileInfo) Size() int64        { return i.size }
func (i logFileInfo) Mode() os.FileMode  { return i.mode }
func (i logFileInfo) ModTime() time.Time { return time.Time{} }
func (i logFileInfo) IsDir() bool        { return i.mode.IsDir() }
func (i logFileInfo) Sys() any           { return nil }
