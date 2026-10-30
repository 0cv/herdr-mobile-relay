package atomicfile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func atomicFixtureDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "herdr-atomicfile-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove fixture directory: %v", err)
		}
	})
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

type recordingOps struct {
	events       []string
	fail         map[string]error
	short        bool
	renameAfter  bool
	createDir    string
	pattern      string
	tempPath     string
	resolvedTemp string
	resolveErr   error
	renameSource string
	renameTarget string
	openDir      string
	removePath   string
}

func (r *recordingOps) operations() operations {
	return operations{
		createTemp: func(dir, pattern string) (tempFile, error) {
			r.events = append(r.events, "create")
			r.createDir, r.pattern = dir, pattern
			if err := r.fail["create"]; err != nil {
				return nil, err
			}
			f, err := os.CreateTemp(dir, pattern)
			if err != nil {
				return nil, err
			}
			r.tempPath = f.Name()
			r.resolvedTemp, r.resolveErr = filepath.EvalSymlinks(r.tempPath)
			return &recordingTemp{File: f, ops: r}, nil
		},
		rename: func(from, to string) error {
			r.events = append(r.events, "rename")
			r.renameSource, r.renameTarget = from, to
			if err := r.fail["rename"]; err != nil && !r.renameAfter {
				return err
			}
			if err := os.Rename(from, to); err != nil {
				return err
			}
			return r.fail["rename"]
		},
		open: func(dir string) (directoryFile, error) {
			r.events = append(r.events, "dir-open")
			r.openDir = dir
			if err := r.fail["dir-open"]; err != nil {
				return nil, err
			}
			f, err := os.Open(dir)
			if err != nil {
				return nil, err
			}
			return &recordingDir{File: f, ops: r}, nil
		},
		remove: func(name string) error {
			r.events = append(r.events, "remove")
			r.removePath = name
			if err := r.fail["remove"]; err != nil {
				return err
			}
			return os.Remove(name)
		},
	}
}

type recordingTemp struct {
	*os.File
	ops *recordingOps
}

func (f *recordingTemp) Chmod(mode os.FileMode) error {
	f.ops.events = append(f.ops.events, "chmod")
	if err := f.ops.fail["chmod"]; err != nil {
		return err
	}
	return f.File.Chmod(mode)
}

func (f *recordingTemp) Write(data []byte) (int, error) {
	f.ops.events = append(f.ops.events, "write")
	if err := f.ops.fail["write"]; err != nil {
		return 0, err
	}
	if f.ops.short {
		return f.File.Write(data[:len(data)-1])
	}
	return f.File.Write(data)
}

func (f *recordingTemp) Sync() error {
	f.ops.events = append(f.ops.events, "file-sync")
	if err := f.ops.fail["file-sync"]; err != nil {
		return err
	}
	return f.File.Sync()
}

func (f *recordingTemp) Close() error {
	f.ops.events = append(f.ops.events, "file-close")
	err := f.File.Close()
	if injected := f.ops.fail["file-close"]; injected != nil {
		return injected
	}
	return err
}

type recordingDir struct {
	*os.File
	ops *recordingOps
}

func (f *recordingDir) Sync() error {
	f.ops.events = append(f.ops.events, "dir-sync")
	if err := f.ops.fail["dir-sync"]; err != nil {
		return err
	}
	return f.File.Sync()
}

func (f *recordingDir) Close() error {
	f.ops.events = append(f.ops.events, "dir-close")
	err := f.File.Close()
	if injected := f.ops.fail["dir-close"]; injected != nil {
		return injected
	}
	return err
}

func TestReplaceFaultsAndPrecedence(t *testing.T) {
	cases := []struct {
		name        string
		fail        []string
		short       bool
		renameAfter bool
		wantErr     string
		outcome     Outcome
		newTarget   bool
		leavesTemp  bool
		events      string
	}{
		{"create", []string{"create"}, false, false, "create", RenameNotConfirmed, false, false, "create"},
		{"chmod-beats-close-and-remove", []string{"chmod", "file-close", "remove"}, false, false, "chmod", RenameNotConfirmed, false, true, "create chmod file-close remove"},
		{"write-error-beats-short-count-and-close", []string{"write", "file-close"}, false, false, "write", RenameNotConfirmed, false, false, "create chmod write file-close remove"},
		{"short-write-beats-close", []string{"file-close"}, true, false, "short", RenameNotConfirmed, false, false, "create chmod write file-close remove"},
		{"file-sync-beats-close", []string{"file-sync", "file-close"}, false, false, "file-sync", RenameNotConfirmed, false, false, "create chmod write file-sync file-close remove"},
		{"file-close", []string{"file-close"}, false, false, "file-close", RenameNotConfirmed, false, false, "create chmod write file-sync file-close remove"},
		{"rename-before-real-rename", []string{"rename"}, false, false, "rename", RenameNotConfirmed, false, false, "create chmod write file-sync file-close rename remove"},
		{"rename-success-then-reported-error-simulation", []string{"rename"}, false, true, "rename", RenameNotConfirmed, true, false, "create chmod write file-sync file-close rename remove"},
		{"directory-open", []string{"dir-open"}, false, false, "dir-open", Renamed, true, false, "create chmod write file-sync file-close rename dir-open remove"},
		{"directory-sync-beats-ignored-close", []string{"dir-sync", "dir-close"}, false, false, "dir-sync", Renamed, true, false, "create chmod write file-sync file-close rename dir-open dir-sync dir-close remove"},
		{"directory-close-ignored", []string{"dir-close"}, false, false, "", Renamed, true, false, "create chmod write file-sync file-close rename dir-open dir-sync dir-close remove"},
		{"remove-failure-after-success-ignored", []string{"remove"}, false, false, "", Renamed, true, false, "create chmod write file-sync file-close rename dir-open dir-sync dir-close remove"},
		{"success", nil, false, false, "", Renamed, true, false, "create chmod write file-sync file-close rename dir-open dir-sync dir-close remove"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := atomicFixtureDir(t)
			target := filepath.Join(dir, "target")
			if err := os.WriteFile(target, []byte("old bytes"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(target, 0o600); err != nil {
				t.Fatal(err)
			}
			r := &recordingOps{fail: make(map[string]error), short: tc.short, renameAfter: tc.renameAfter}
			for _, stage := range tc.fail {
				r.fail[stage] = errors.New(stage + " failure")
			}
			outcome, err := replace(dir, target, ".stage.*.tmp", []byte("new bytes"), 0o600, r.operations())
			var wantErr error
			if tc.wantErr == "short" {
				wantErr = io.ErrShortWrite
			} else if tc.wantErr != "" {
				wantErr = r.fail[tc.wantErr]
			}
			if outcome != tc.outcome || err != wantErr {
				t.Fatalf("outcome, error = %v, %v; want %v, %v", outcome, err, tc.outcome, wantErr)
			}
			if got := strings.Join(r.events, " "); got != tc.events {
				t.Fatalf("events = %q, want %q", got, tc.events)
			}
			if r.createDir != dir || r.pattern != ".stage.*.tmp" {
				t.Fatalf("create arguments = %q, %q", r.createDir, r.pattern)
			}
			if r.tempPath != "" {
				if r.resolveErr != nil {
					t.Fatalf("resolve temporary path: %v", r.resolveErr)
				}
				if r.removePath != r.tempPath || r.removePath == target {
					t.Fatalf("cleanup path = %q, temp = %q, target = %q", r.removePath, r.tempPath, target)
				}
				_, statErr := os.Stat(r.tempPath)
				if tc.leavesTemp {
					if statErr != nil {
						t.Fatalf("failed cleanup did not leave temp: %v", statErr)
					}
					if err := os.Remove(r.tempPath); err != nil {
						t.Fatalf("fixture cleanup: %v", err)
					}
				} else if !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("temp remains or stat failed: %v", statErr)
				}
			}
			if r.renameSource != "" && (r.renameSource != r.tempPath || r.renameTarget != target) {
				t.Fatalf("rename arguments = %q, %q", r.renameSource, r.renameTarget)
			}
			if r.openDir != "" && r.openDir != dir {
				t.Fatalf("open directory = %q, want %q", r.openDir, dir)
			}
			wantData := "old bytes"
			if tc.newTarget {
				wantData = "new bytes"
			}
			data, readErr := os.ReadFile(target)
			if readErr != nil || string(data) != wantData {
				t.Fatalf("target = %q, error %v; want %q", data, readErr, wantData)
			}
			info, statErr := os.Stat(target)
			if statErr != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("target mode = %v, error %v", info, statErr)
			}
		})
	}
}

func TestReplaceNoncanonicalPathsRemainSeparate(t *testing.T) {
	root := atomicFixtureDir(t)
	physical := filepath.Join(root, "physical")
	child := filepath.Join(physical, "child")
	if err := os.Mkdir(physical, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(physical, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(child, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(child, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	directory := root + string(os.PathSeparator) + "link" + string(os.PathSeparator) + ".."
	target := filepath.Join(directory, "target")
	resolvedDir, err := filepath.EvalSymlinks(directory)
	if err != nil {
		t.Fatal(err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	resolvedPhysical, err := filepath.EvalSymlinks(physical)
	if err != nil {
		t.Fatal(err)
	}
	if resolvedDir != resolvedPhysical || resolvedDir == resolvedRoot || target != filepath.Join(root, "target") {
		t.Fatalf("fixture resolution: directory %q, root %q, physical %q, target %q", resolvedDir, resolvedRoot, resolvedPhysical, target)
	}
	if err := os.WriteFile(target, []byte("old root"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatal(err)
	}
	physicalTarget := filepath.Join(physical, "target")
	if err := os.WriteFile(physicalTarget, []byte("physical sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(physicalTarget, 0o600); err != nil {
		t.Fatal(err)
	}
	r := &recordingOps{fail: make(map[string]error)}
	outcome, err := replace(directory, target, ".raw.*.tmp", []byte("new root"), 0o600, r.operations())
	if outcome != Renamed || err != nil {
		t.Fatalf("replace = %v, %v", outcome, err)
	}
	if r.createDir != directory || r.openDir != directory || r.pattern != ".raw.*.tmp" || r.renameSource != r.tempPath || r.renameTarget != target || r.removePath != r.tempPath {
		t.Fatalf("recorded paths: %+v", r)
	}
	if r.resolveErr != nil || filepath.Dir(r.resolvedTemp) != resolvedDir {
		t.Fatalf("temporary path %q resolved to %q, error %v; want parent %q", r.tempPath, r.resolvedTemp, r.resolveErr, resolvedDir)
	}
	for path, want := range map[string]string{target: "new root", physicalTarget: "physical sentinel"} {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != want {
			t.Fatalf("%s = %q, error %v; want %q", path, data, err, want)
		}
	}
}

func TestReplacePublicMode(t *testing.T) {
	dir := atomicFixtureDir(t)
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatal(err)
	}
	outcome, err := Replace(dir, target, ".mode.*.tmp", []byte("new"), 0o640)
	if outcome != Renamed || err != nil {
		t.Fatalf("replace = %v, %v", outcome, err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "new" {
		t.Fatalf("target = %q, error %v; want new", data, err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("target mode = %o, want 640", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("list fixture directory: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "target" {
		t.Fatalf("fixture entries = %v, want only target", entries)
	}
}

func TestReplaceNativeVisibility(t *testing.T) {
	dir := atomicFixtureDir(t)
	target := filepath.Join(dir, "target")
	old := bytes.Repeat([]byte("old-payload-"), 8192)
	newData := bytes.Repeat([]byte("new-payload-"), 8192)
	if err := os.WriteFile(target, old, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	observed := make(chan struct{}, 1)
	readerErr := make(chan error, 1)
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			data, err := os.ReadFile(target)
			if err != nil {
				readerErr <- err
				return
			}
			if !bytes.Equal(data, old) && !bytes.Equal(data, newData) {
				readerErr <- fmt.Errorf("reader saw incomplete or mixed payload: %d bytes", len(data))
				return
			}
			select {
			case observed <- struct{}{}:
			default:
			}
		}
	}()
	defer func() {
		close(stop)
		<-done
		select {
		case err := <-readerErr:
			t.Errorf("reader: %v", err)
		default:
		}
	}()
	waitObservation := func() {
		t.Helper()
		select {
		case <-observed:
		case err := <-readerErr:
			t.Fatalf("reader: %v", err)
		}
	}
	waitObservation()
	for i := 0; i < 24; i++ {
		data := newData
		if i%2 != 0 {
			data = old
		}
		outcome, err := Replace(dir, target, ".visibility.*.tmp", data, 0o600)
		if outcome != Renamed || err != nil {
			t.Fatalf("replacement %d = %v, %v", i, outcome, err)
		}
		waitObservation()
	}
	select {
	case err := <-readerErr:
		t.Fatalf("reader: %v", err)
	default:
	}
	data, err := os.ReadFile(target)
	if err != nil || !reflect.DeepEqual(data, old) {
		t.Fatalf("final target = %d bytes, error %v", len(data), err)
	}
}
