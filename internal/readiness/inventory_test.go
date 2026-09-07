package readiness

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublishReplacesExpectedInventoryDurably(t *testing.T) {
	root := t.TempDir()
	session := filepath.Join(root, "sessions", "g1")
	if err := os.MkdirAll(session, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(session, "expected-inventory.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"generation":"g1","acknowledged_empty":true,"panes":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	panes := []Pane{{PaneID: "pane-2", NativeSessionID: "session-2", ProfileID: "emu"}, {PaneID: "pane-1", NativeSessionID: "session-1", ProfileID: "personal"}}
	if err := Publish(path, "g1", panes, false); err != nil {
		t.Fatal(err)
	}
	if result := Check(path, "g1", panes); !result.Ready || result.State != StateReady {
		t.Fatalf("published inventory = %+v", result)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"version":1,"generation":"g1","acknowledged_empty":false,"panes":[{"pane_id":"pane-1","native_session_id":"session-1","profile_id":"personal"},{"pane_id":"pane-2","native_session_id":"session-2","profile_id":"emu"}]}`+"\n" {
		t.Fatalf("published bytes = %q", data)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("published mode = (%v, %v)", info, err)
	}
}

func TestPublishRejectsInvalidInventoryBeforeReplacingThePriorManifest(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "expected-inventory.json")
	prior := []byte(`{"version":1,"generation":"g1","acknowledged_empty":true,"panes":[]}`)
	if err := os.WriteFile(path, prior, 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		path       string
		generation string
		panes      []Pane
		empty      bool
	}{
		{path: "relative/expected-inventory.json", generation: "g1"},
		{path: path, generation: ""},
		{path: path, generation: "g1", panes: []Pane{{PaneID: "pane", NativeSessionID: "same", ProfileID: "a"}, {PaneID: "other", NativeSessionID: "same", ProfileID: "b"}}},
		{path: path, generation: "g1", panes: []Pane{{PaneID: "pane", NativeSessionID: "session", ProfileID: "profile"}}, empty: true},
	}
	for index, test := range tests {
		if err := Publish(test.path, test.generation, test.panes, test.empty); err == nil {
			t.Errorf("invalid case %d was published", index)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(prior) {
		t.Fatalf("prior manifest changed to %q", data)
	}
}

func TestPublishCoversEveryDurableReplacementBoundary(t *testing.T) {
	baseDirectory := t.TempDir()
	if err := os.Chmod(baseDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(baseDirectory, "expected-inventory.json")
	directoryInfo, err := os.Lstat(baseDirectory)
	if err != nil {
		t.Fatal(err)
	}
	fileInfo := publishRegularInfo(t)
	base := func() (publishIO, *fakePublishRoot, *fakePublishFile) {
		file := &fakePublishFile{info: fileInfo}
		root := &fakePublishRoot{
			file: file, directory: &fakePublishDirectory{}, lstatErr: os.ErrNotExist, statInfo: directoryInfo,
		}
		return publishIO{
			lstat:      func(string) (os.FileInfo, error) { return directoryInfo, nil },
			openRoot:   func(string) (publishRoot, error) { return root, nil },
			marshal:    json.Marshal,
			randomName: func() string { return "temporary" },
		}, root, file
	}
	tests := []struct {
		name   string
		mutate func(*publishIO, *fakePublishRoot, *fakePublishFile)
	}{
		{name: "marshal", mutate: func(ops *publishIO, _ *fakePublishRoot, _ *fakePublishFile) {
			ops.marshal = func(any) ([]byte, error) { return nil, errors.New("marshal") }
		}},
		{name: "directory lstat", mutate: func(ops *publishIO, _ *fakePublishRoot, _ *fakePublishFile) {
			ops.lstat = func(string) (os.FileInfo, error) { return nil, errors.New("lstat") }
		}},
		{name: "unsafe directory", mutate: func(ops *publishIO, _ *fakePublishRoot, _ *fakePublishFile) {
			ops.lstat = func(string) (os.FileInfo, error) { return fileInfo, nil }
		}},
		{name: "open directory", mutate: func(ops *publishIO, _ *fakePublishRoot, _ *fakePublishFile) {
			ops.openRoot = func(string) (publishRoot, error) { return nil, errors.New("open") }
		}},
		{name: "directory stat", mutate: func(_ *publishIO, root *fakePublishRoot, _ *fakePublishFile) { root.statErr = errors.New("stat") }},
		{name: "changed directory", mutate: func(_ *publishIO, root *fakePublishRoot, _ *fakePublishFile) { root.statInfo = publishDirectoryInfo(t) }},
		{name: "unsafe directory after open", mutate: func(_ *publishIO, root *fakePublishRoot, _ *fakePublishFile) { root.statInfo = fileInfo }},
		{name: "unsafe existing manifest", mutate: func(_ *publishIO, root *fakePublishRoot, _ *fakePublishFile) {
			root.lstatInfo, root.lstatErr = publishDirectoryInfo(t), nil
		}},
		{name: "inspect existing manifest", mutate: func(_ *publishIO, root *fakePublishRoot, _ *fakePublishFile) { root.lstatErr = errors.New("inspect") }},
		{name: "create temporary", mutate: func(_ *publishIO, root *fakePublishRoot, _ *fakePublishFile) { root.openFileErr = errors.New("create") }},
		{name: "temporary stat", mutate: func(_ *publishIO, _ *fakePublishRoot, file *fakePublishFile) { file.statErr = errors.New("stat") }},
		{name: "unsafe temporary", mutate: func(_ *publishIO, _ *fakePublishRoot, file *fakePublishFile) { file.info = publishDirectoryInfo(t) }},
		{name: "temporary write", mutate: func(_ *publishIO, _ *fakePublishRoot, file *fakePublishFile) { file.writeErr = errors.New("write") }},
		{name: "short temporary write", mutate: func(_ *publishIO, _ *fakePublishRoot, file *fakePublishFile) { file.shortWrite = true }},
		{name: "temporary sync", mutate: func(_ *publishIO, _ *fakePublishRoot, file *fakePublishFile) { file.syncErr = errors.New("sync") }},
		{name: "temporary close", mutate: func(_ *publishIO, _ *fakePublishRoot, file *fakePublishFile) { file.closeErr = errors.New("close") }},
		{name: "rename", mutate: func(_ *publishIO, root *fakePublishRoot, _ *fakePublishFile) { root.renameErr = errors.New("rename") }},
		{name: "directory reopen", mutate: func(_ *publishIO, root *fakePublishRoot, _ *fakePublishFile) {
			root.openDirectoryErr = errors.New("open")
		}},
		{name: "directory sync", mutate: func(_ *publishIO, root *fakePublishRoot, _ *fakePublishFile) {
			root.directory.(*fakePublishDirectory).syncErr = errors.New("sync")
		}},
		{name: "directory close", mutate: func(_ *publishIO, root *fakePublishRoot, _ *fakePublishFile) {
			root.directory.(*fakePublishDirectory).closeErr = errors.New("close")
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ops, root, file := base()
			test.mutate(&ops, root, file)
			if err := publishWith(ops, path, "g1", []Pane{{PaneID: "p", NativeSessionID: "s", ProfileID: "profile"}}, false); err == nil {
				t.Fatal("publish boundary failure was ignored")
			}
		})
	}
	ops, _, _ := base()
	ops.marshal = func(any) ([]byte, error) { return make([]byte, maxManifestBytes+1), nil }
	if err := publishWith(ops, path, "g1", nil, true); err == nil {
		t.Fatal("oversized manifest was published")
	}
	ops, _, _ = base()
	if err := publishWith(ops, path, "g1", nil, true); err != nil {
		t.Fatalf("acknowledged empty publish = %v", err)
	}
	if opened, err := defaultPublishIO().openRoot(filepath.Join(baseDirectory, "missing")); err == nil || opened != nil {
		t.Fatalf("default missing root = (%v, %v)", opened, err)
	}
}

func TestPublishValidationCoversEveryPathGenerationAndFileShape(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "expected-inventory.json")
	for _, test := range []struct {
		path       string
		generation string
	}{
		{path: "relative/expected-inventory.json", generation: "g"},
		{path: filepath.Join(root, "nested", "..", "expected-inventory.json"), generation: "g"},
		{path: filepath.Join(root, "other.json"), generation: "g"},
		{path: path, generation: " g"},
		{path: path, generation: "bad\n"},
	} {
		if err := Publish(test.path, test.generation, nil, true); err == nil {
			t.Fatalf("invalid publish identity was accepted: %+v", test)
		}
	}
	regular := publishRegularInfo(t)
	directory := publishDirectoryInfo(t)
	if !validPublishDirectory(directory) || validPublishDirectory(regular) {
		t.Fatal("publish directory validation was not exact")
	}
	if !validPublishManifestFile(regular) || validPublishManifestFile(directory) {
		t.Fatal("publish file validation was not exact")
	}
}

func TestExactExpectedInventory(t *testing.T) {
	manifest := writeManifest(t, `{"version":1,"generation":"generation-7","panes":[{"pane_id":"pane-personal","native_session_id":"session-personal","profile_id":"personal"},{"pane_id":"pane-emu","native_session_id":"session-emu","profile_id":"emu"}]}`)
	observed := []Pane{
		{PaneID: "pane-emu", NativeSessionID: "session-emu", ProfileID: "emu"},
		{PaneID: "pane-personal", NativeSessionID: "session-personal", ProfileID: "personal"},
	}
	result := Check(manifest, "generation-7", observed)
	if !result.Ready || result.State != StateReady || result.Expected != 2 || result.Observed != 2 {
		t.Fatalf("exact inventory result = %+v", result)
	}
}

func TestAcknowledgedEmptyIsGenerationBoundAndDistinct(t *testing.T) {
	manifest := writeManifest(t, `{"version":1,"generation":"generation-empty","acknowledged_empty":true,"panes":[]}`)
	if result := Check(manifest, "generation-empty", nil); !result.Ready || result.State != StateAcknowledgedEmpty {
		t.Fatalf("acknowledged empty result = %+v", result)
	}
	if result := Check(manifest, "other-generation", nil); result.Ready || result.State != StateGenerationMismatch {
		t.Fatalf("cross-generation acknowledgement result = %+v", result)
	}
}

func TestExpectedInventoryFailsClosed(t *testing.T) {
	tests := []struct {
		name       string
		manifest   string
		generation string
		observed   []Pane
		want       State
	}{
		{name: "unexpected empty", manifest: `{"version":1,"generation":"g","panes":[{"pane_id":"p","native_session_id":"s","profile_id":"personal"}]}`, generation: "g", want: StateUnexpectedEmpty},
		{name: "partial", manifest: `{"version":1,"generation":"g","panes":[{"pane_id":"p1","native_session_id":"s1","profile_id":"personal"},{"pane_id":"p2","native_session_id":"s2","profile_id":"emu"}]}`, generation: "g", observed: []Pane{{PaneID: "p1", NativeSessionID: "s1", ProfileID: "personal"}}, want: StateInventoryMismatch},
		{name: "extra", manifest: `{"version":1,"generation":"g","panes":[]}`, generation: "g", observed: []Pane{{PaneID: "p", NativeSessionID: "s", ProfileID: "personal"}}, want: StateInventoryMismatch},
		{name: "wrong native session", manifest: `{"version":1,"generation":"g","panes":[{"pane_id":"p","native_session_id":"expected","profile_id":"personal"}]}`, generation: "g", observed: []Pane{{PaneID: "p", NativeSessionID: "other", ProfileID: "personal"}}, want: StateInventoryMismatch},
		{name: "wrong profile", manifest: `{"version":1,"generation":"g","panes":[{"pane_id":"p","native_session_id":"s","profile_id":"personal"}]}`, generation: "g", observed: []Pane{{PaneID: "p", NativeSessionID: "s", ProfileID: "emu"}}, want: StateInventoryMismatch},
		{name: "empty manifest not acknowledged", manifest: `{"version":1,"generation":"g","panes":[]}`, generation: "g", want: StateUnexpectedEmpty},
		{name: "ack with pane", manifest: `{"version":1,"generation":"g","acknowledged_empty":true,"panes":[{"pane_id":"p","native_session_id":"s","profile_id":"personal"}]}`, generation: "g", want: StateInvalidManifest},
		{name: "duplicate expected pane", manifest: `{"version":1,"generation":"g","panes":[{"pane_id":"p","native_session_id":"s1","profile_id":"personal"},{"pane_id":"p","native_session_id":"s2","profile_id":"emu"}]}`, generation: "g", want: StateInvalidManifest},
		{name: "duplicate observed session", manifest: `{"version":1,"generation":"g","panes":[{"pane_id":"p1","native_session_id":"s","profile_id":"personal"}]}`, generation: "g", observed: []Pane{{PaneID: "p1", NativeSessionID: "s", ProfileID: "personal"}, {PaneID: "p2", NativeSessionID: "s", ProfileID: "emu"}}, want: StateInvalidInventory},
		{name: "newer format", manifest: `{"version":2,"generation":"g","panes":[]}`, generation: "g", want: StateInvalidManifest},
		{name: "corrupt", manifest: `{`, generation: "g", want: StateInvalidManifest},
		{name: "unknown field", manifest: `{"version":1,"generation":"g","panes":[],"future":true}`, generation: "g", want: StateInvalidManifest},
		{name: "trailing value", manifest: `{"version":1,"generation":"g","panes":[]} {}`, generation: "g", want: StateInvalidManifest},
		{name: "trailing invalid", manifest: `{"version":1,"generation":"g","panes":[]} nope`, generation: "g", want: StateInvalidManifest},
		{name: "blank observed pane", manifest: `{"version":1,"generation":"g","panes":[{"pane_id":"p","native_session_id":"s","profile_id":"personal"}]}`, generation: "g", observed: []Pane{{PaneID: " ", NativeSessionID: "s", ProfileID: "personal"}}, want: StateInvalidInventory},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := Check(writeManifest(t, test.manifest), test.generation, test.observed)
			if result.Ready || result.State != test.want {
				t.Fatalf("result = %+v, want %s", result, test.want)
			}
		})
	}
}

func TestExpectedInventoryRejectsMissingAndPublicFiles(t *testing.T) {
	if result := Check(filepath.Join(t.TempDir(), "missing.json"), "g", nil); result.State != StateUnavailable {
		t.Fatalf("missing manifest result = %+v", result)
	}
	path := writeManifest(t, `{"version":1,"generation":"g","acknowledged_empty":true,"panes":[]}`)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if result := Check(path, "g", nil); result.State != StateInvalidManifest {
		t.Fatalf("public manifest result = %+v", result)
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	if result := Check(path, "g", nil); result.State != StateUnavailable {
		t.Fatalf("unreadable manifest result = %+v", result)
	}
}

func TestExpectedInventoryRejectsUnsafeFilesystemShapes(t *testing.T) {
	valid := `{"version":1,"generation":"g","acknowledged_empty":true,"panes":[]}`
	t.Run("same-inode symlink swap", func(t *testing.T) {
		path := writeManifest(t, valid)
		moved := filepath.Join(filepath.Dir(path), "moved")
		ops := defaultCheckIO()
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
		if result := checkWith(ops, path, "g", nil); result.Ready || result.State == StateReady {
			t.Fatalf("same-inode symlink swap result = %+v", result)
		}
	})
	t.Run("hardlink", func(t *testing.T) {
		path := writeManifest(t, valid)
		if err := os.Link(path, filepath.Join(filepath.Dir(path), "second-link")); err != nil {
			t.Fatal(err)
		}
		if result := Check(path, "g", nil); result.State != StateInvalidManifest {
			t.Fatalf("hardlinked manifest result = %+v", result)
		}
	})
	t.Run("oversized", func(t *testing.T) {
		path := writeManifest(t, strings.Repeat("x", maxManifestBytes+1))
		if result := Check(path, "g", nil); result.State != StateInvalidManifest {
			t.Fatalf("oversized manifest result = %+v", result)
		}
	})
}

func TestExpectedInventoryFailsClosedAtEveryOpenReadBoundary(t *testing.T) {
	valid := `{"version":1,"generation":"g","acknowledged_empty":true,"panes":[]}`
	path := writeManifest(t, valid)
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(filepath.Dir(path), "other")
	if err := os.WriteFile(other, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	otherInfo, err := os.Lstat(other)
	if err != nil {
		t.Fatal(err)
	}
	directoryInfo, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}

	tests := map[string]struct {
		ops  checkIO
		want State
	}{
		"lstat": {
			ops:  checkIO{lstat: func(string) (os.FileInfo, error) { return nil, errors.New("lstat") }},
			want: StateUnavailable,
		},
		"open": {
			ops: checkIO{
				lstat: func(string) (os.FileInfo, error) { return info, nil },
				open:  func(string) (manifestFile, error) { return nil, errors.New("open") },
			},
			want: StateUnavailable,
		},
		"stat": {
			ops: checkIO{
				lstat: func(string) (os.FileInfo, error) { return info, nil },
				open:  func(string) (manifestFile, error) { return &fixtureManifestFile{statErr: errors.New("stat")}, nil },
			},
			want: StateUnavailable,
		},
		"unsafe after open": {
			ops: checkIO{
				lstat: func(string) (os.FileInfo, error) { return info, nil },
				open:  func(string) (manifestFile, error) { return &fixtureManifestFile{info: directoryInfo}, nil },
			},
			want: StateInvalidManifest,
		},
		"changed after open": {
			ops: checkIO{
				lstat: func(string) (os.FileInfo, error) { return info, nil },
				open:  func(string) (manifestFile, error) { return &fixtureManifestFile{info: otherInfo}, nil },
			},
			want: StateInvalidManifest,
		},
		"read": {
			ops: checkIO{
				lstat: func(string) (os.FileInfo, error) { return info, nil },
				open: func(string) (manifestFile, error) {
					return &fixtureManifestFile{info: info, readErr: errors.New("read")}, nil
				},
			},
			want: StateUnavailable,
		},
		"close": {
			ops: checkIO{
				lstat: func(string) (os.FileInfo, error) { return info, nil },
				open: func(string) (manifestFile, error) {
					return &fixtureManifestFile{info: info, reader: strings.NewReader(valid), closeErr: errors.New("close")}, nil
				},
			},
			want: StateUnavailable,
		},
		"growth after stat": {
			ops: checkIO{
				lstat: func(string) (os.FileInfo, error) { return info, nil },
				open: func(string) (manifestFile, error) {
					return &fixtureManifestFile{info: info, reader: strings.NewReader(strings.Repeat("x", maxManifestBytes+1))}, nil
				},
			},
			want: StateInvalidManifest,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			result := checkWith(test.ops, path, "g", nil)
			if result.Ready || result.State != test.want {
				t.Fatalf("result = %+v, want %s", result, test.want)
			}
		})
	}
}

func TestReadinessValidationHelpers(t *testing.T) {
	for _, value := range []string{"", strings.Repeat("x", 257), "value\n", "value\x7f"} {
		if validValue(value) {
			t.Fatalf("invalid value accepted: %q", value)
		}
	}
	if !validValue("value") {
		t.Fatal("valid value rejected")
	}
	if !samePaneSet(map[string]Pane{}, map[string]Pane{}) {
		t.Fatal("empty pane sets did not match")
	}

	for _, input := range []string{"{}", "nope"} {
		decoder := json.NewDecoder(bytes.NewBufferString(input))
		if err := trailingJSON(decoder); err == nil {
			t.Fatalf("trailing JSON %q accepted", input)
		}
	}
}

func writeManifest(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "expected-inventory.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type fixtureManifestFile struct {
	reader   io.Reader
	info     os.FileInfo
	statErr  error
	readErr  error
	closeErr error
}

func (f *fixtureManifestFile) Read(data []byte) (int, error) {
	if f.readErr != nil {
		return 0, f.readErr
	}
	if f.reader == nil {
		return 0, io.EOF
	}
	return f.reader.Read(data)
}

func (f *fixtureManifestFile) Stat() (os.FileInfo, error) { return f.info, f.statErr }
func (f *fixtureManifestFile) Close() error               { return f.closeErr }

type fakePublishRoot struct {
	file             publishFile
	openFileErr      error
	directory        publishDirectory
	openDirectoryErr error
	lstatInfo        os.FileInfo
	lstatErr         error
	statInfo         os.FileInfo
	statErr          error
	renameErr        error
	removeErr        error
	closeErr         error
}

func (r *fakePublishRoot) OpenFile(string, int, os.FileMode) (publishFile, error) {
	return r.file, r.openFileErr
}
func (r *fakePublishRoot) OpenDirectory(string) (publishDirectory, error) {
	return r.directory, r.openDirectoryErr
}
func (r *fakePublishRoot) Lstat(string) (os.FileInfo, error) { return r.lstatInfo, r.lstatErr }
func (r *fakePublishRoot) Stat(string) (os.FileInfo, error)  { return r.statInfo, r.statErr }
func (r *fakePublishRoot) Rename(string, string) error       { return r.renameErr }
func (r *fakePublishRoot) Remove(string) error               { return r.removeErr }
func (r *fakePublishRoot) Close() error                      { return r.closeErr }

type fakePublishFile struct {
	info       os.FileInfo
	statErr    error
	writeErr   error
	shortWrite bool
	syncErr    error
	closeErr   error
}

func (f *fakePublishFile) Write(data []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	if f.shortWrite {
		return len(data) - 1, nil
	}
	return len(data), nil
}
func (f *fakePublishFile) Stat() (os.FileInfo, error) { return f.info, f.statErr }
func (f *fakePublishFile) Sync() error                { return f.syncErr }
func (f *fakePublishFile) Close() error               { return f.closeErr }

type fakePublishDirectory struct {
	syncErr  error
	closeErr error
}

func (d *fakePublishDirectory) Sync() error  { return d.syncErr }
func (d *fakePublishDirectory) Close() error { return d.closeErr }

func publishRegularInfo(t *testing.T) os.FileInfo {
	t.Helper()
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func publishDirectoryInfo(t *testing.T) os.FileInfo {
	t.Helper()
	path := t.TempDir()
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}
