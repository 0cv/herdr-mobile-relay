package profiles

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCustomPaneAssociationsSurviveResolverRestart(t *testing.T) {
	configHome, stateDir := configuredCustomProfiles(t)
	observed := []Observation{
		{PaneID: "pane-personal", NativeSessionID: "session-personal"},
		{PaneID: "pane-emu", NativeSessionID: "session-emu"},
	}

	first := NewResolver(configHome, nil, WithAssociationStore(stateDir))
	first.Remember("pane-personal", "personal")
	first.Remember("pane-emu", "emu")
	if err := first.Reconcile(observed); err != nil {
		t.Fatalf("persist associations: %v", err)
	}

	restarted := NewResolver(configHome, nil, WithAssociationStore(stateDir))
	if err := restarted.Reconcile(observed); err != nil {
		t.Fatalf("reload associations: %v", err)
	}
	if got := restarted.ResolvePane("pane-personal", "copilot"); got != "personal" {
		t.Fatalf("personal pane resolved to %q", got)
	}
	if got := restarted.ResolvePane("pane-emu", "copilot"); got != "emu" {
		t.Fatalf("EMU pane resolved to %q", got)
	}
	info, err := os.Stat(filepath.Join(stateDir, associationStoreName))
	if err != nil {
		t.Fatalf("stat association store: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("association store mode = %o, want 600", info.Mode().Perm())
	}
}

func TestAssociationReconcileRejectsConflictAndPrunesStaleEntries(t *testing.T) {
	configHome, stateDir := configuredCustomProfiles(t)
	store := filepath.Join(stateDir, associationStoreName)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"version":1,"associations":[{"pane_id":"pane-live","native_session_id":"session-live","profile_id":"personal"},{"pane_id":"pane-live","native_session_id":"session-live","profile_id":"emu"},{"pane_id":"pane-stale","native_session_id":"session-stale","profile_id":"personal"}]}`)
	if err := os.WriteFile(store, data, 0o600); err != nil {
		t.Fatal(err)
	}

	resolver := NewResolver(configHome, nil, WithAssociationStore(stateDir))
	err := resolver.Reconcile([]Observation{{PaneID: "pane-live", NativeSessionID: "session-live"}})
	if err == nil {
		t.Fatal("conflicting association store was accepted")
	}
	if got := resolver.ResolvePane("pane-live", "copilot"); got != "" {
		t.Fatalf("conflicting pane resolved to %q, want unknown", got)
	}
	if got := resolver.ResolvePane("pane-stale", "copilot"); got != "" {
		t.Fatalf("stale pane resolved to %q, want unknown", got)
	}
}

func TestAssociationRequiresExactNativeSessionAndConfiguredProfile(t *testing.T) {
	configHome, stateDir := configuredCustomProfiles(t)
	resolver := NewResolver(configHome, nil, WithAssociationStore(stateDir))
	resolver.Remember("pane", "personal")
	if err := resolver.Reconcile([]Observation{{PaneID: "pane", NativeSessionID: "session-one"}}); err != nil {
		t.Fatal(err)
	}

	restarted := NewResolver(configHome, nil, WithAssociationStore(stateDir))
	if err := restarted.Reconcile([]Observation{{PaneID: "pane", NativeSessionID: "session-two"}}); err != nil {
		t.Fatal(err)
	}
	if got := restarted.ResolvePane("pane", "copilot"); got != "" {
		t.Fatalf("changed native session resolved to %q, want unknown", got)
	}
}

func TestAssociationReconcilePrunesRemovedProfileAndPersistsEmptyStore(t *testing.T) {
	configHome, stateDir := configuredCustomProfiles(t)
	resolver := NewResolver(configHome, nil, WithAssociationStore(stateDir))
	resolver.Remember(" PANE ", " PERSONAL ")
	observed := []Observation{{PaneID: "pane", NativeSessionID: "session"}}
	if err := resolver.Reconcile(observed); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configHome, "herdr", "agent-profiles.ini"), []byte("[config]\nreplace_profiles = true\n[profiles]\nemu = EMU\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver.Reload()
	if err := resolver.Reconcile(observed); err != nil {
		t.Fatal(err)
	}
	if got := resolver.ResolvePane("pane", "copilot"); got != "" {
		t.Fatalf("removed profile resolved to %q", got)
	}
	data, err := os.ReadFile(filepath.Join(stateDir, associationStoreName))
	if err != nil {
		t.Fatal(err)
	}
	var store associationStore
	if err := json.Unmarshal(data, &store); err != nil {
		t.Fatal(err)
	}
	if len(store.Associations) != 0 {
		t.Fatalf("pruned store = %+v", store.Associations)
	}
}

func TestAssociationReconcileRejectsInvalidAndAmbiguousObservations(t *testing.T) {
	configHome, stateDir := configuredCustomProfiles(t)
	tests := []struct {
		name     string
		observed []Observation
	}{
		{name: "empty pane", observed: []Observation{{NativeSessionID: "session"}}},
		{name: "empty session", observed: []Observation{{PaneID: "pane"}}},
		{name: "control pane", observed: []Observation{{PaneID: "pane\n", NativeSessionID: "session"}}},
		{name: "control session", observed: []Observation{{PaneID: "pane", NativeSessionID: "session\x7f"}}},
		{name: "long pane", observed: []Observation{{PaneID: strings.Repeat("p", 257), NativeSessionID: "session"}}},
		{name: "duplicate pane", observed: []Observation{{PaneID: "pane", NativeSessionID: "one"}, {PaneID: "PANE", NativeSessionID: "two"}}},
		{name: "duplicate session", observed: []Observation{{PaneID: "one", NativeSessionID: "session"}, {PaneID: "two", NativeSessionID: "session"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolver := NewResolver(configHome, nil, WithAssociationStore(stateDir))
			if err := resolver.Reconcile(test.observed); err == nil {
				t.Fatal("invalid observations were accepted")
			}
		})
	}
}

func TestAssociationStoreFailsClosedForInvalidFiles(t *testing.T) {
	configHome, _ := configuredCustomProfiles(t)
	tests := []struct {
		name    string
		content string
	}{
		{name: "empty", content: ""},
		{name: "trailing value", content: `{"version":1,"associations":[]} {}`},
		{name: "trailing invalid", content: `{"version":1,"associations":[]} nope`},
		{name: "unknown field", content: `{"version":1,"associations":[],"future":true}`},
		{name: "wrong version", content: `{"version":2,"associations":[]}`},
		{name: "empty pane", content: `{"version":1,"associations":[{"pane_id":"","native_session_id":"s","profile_id":"personal"}]}`},
		{name: "empty session", content: `{"version":1,"associations":[{"pane_id":"p","native_session_id":"","profile_id":"personal"}]}`},
		{name: "empty profile", content: `{"version":1,"associations":[{"pane_id":"p","native_session_id":"s","profile_id":""}]}`},
		{name: "blank pane", content: `{"version":1,"associations":[{"pane_id":" ","native_session_id":"s","profile_id":"personal"}]}`},
		{name: "blank session", content: `{"version":1,"associations":[{"pane_id":"p","native_session_id":" ","profile_id":"personal"}]}`},
		{name: "blank profile", content: `{"version":1,"associations":[{"pane_id":"p","native_session_id":"s","profile_id":" "}]}`},
		{name: "duplicate pane", content: `{"version":1,"associations":[{"pane_id":"p","native_session_id":"s1","profile_id":"personal"},{"pane_id":"P","native_session_id":"s2","profile_id":"emu"}]}`},
		{name: "duplicate session", content: `{"version":1,"associations":[{"pane_id":"p1","native_session_id":"s","profile_id":"personal"},{"pane_id":"p2","native_session_id":"s","profile_id":"emu"}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stateDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(stateDir, associationStoreName), []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			resolver := NewResolver(configHome, nil, WithAssociationStore(stateDir))
			if err := resolver.Reconcile(nil); err == nil {
				t.Fatal("invalid store was accepted")
			}
		})
	}
}

func TestAssociationStoreRejectsUnsafeAndOversizedPaths(t *testing.T) {
	configHome, _ := configuredCustomProfiles(t)
	t.Run("no store configured", func(t *testing.T) {
		if err := NewResolver(configHome, nil).Reconcile(nil); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("unreadable parent", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "parent")
		if err := os.WriteFile(parent, []byte("file"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := NewResolver(configHome, nil, WithAssociationStore(filepath.Join(parent, "state"))).Reconcile(nil); err == nil {
			t.Fatal("invalid association parent was accepted")
		}
	})
	t.Run("public file", func(t *testing.T) {
		stateDir := t.TempDir()
		path := filepath.Join(stateDir, associationStoreName)
		if err := os.WriteFile(path, []byte(`{"version":1,"associations":[]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := NewResolver(configHome, nil, WithAssociationStore(stateDir)).Reconcile(nil); err == nil {
			t.Fatal("public store was accepted")
		}
	})
	t.Run("unreadable file", func(t *testing.T) {
		stateDir := t.TempDir()
		path := filepath.Join(stateDir, associationStoreName)
		if err := os.WriteFile(path, []byte(`{"version":1,"associations":[]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
		if err := NewResolver(configHome, nil, WithAssociationStore(stateDir)).Reconcile(nil); err == nil {
			t.Fatal("unreadable store was accepted")
		}
	})
	t.Run("store directory", func(t *testing.T) {
		stateDir := t.TempDir()
		if err := os.Mkdir(filepath.Join(stateDir, associationStoreName), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := NewResolver(configHome, nil, WithAssociationStore(stateDir)).Reconcile(nil); err == nil {
			t.Fatal("directory store was accepted")
		}
	})
	t.Run("store symlink", func(t *testing.T) {
		stateDir := t.TempDir()
		target := filepath.Join(stateDir, "target")
		if err := os.WriteFile(target, []byte(`{"version":1,"associations":[]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(stateDir, associationStoreName)); err != nil {
			t.Fatal(err)
		}
		if err := NewResolver(configHome, nil, WithAssociationStore(stateDir)).Reconcile(nil); err == nil {
			t.Fatal("symlink store was accepted")
		}
	})
	t.Run("oversized", func(t *testing.T) {
		stateDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(stateDir, associationStoreName), make([]byte, 1024*1024+1), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := NewResolver(configHome, nil, WithAssociationStore(stateDir)).Reconcile(nil); err == nil {
			t.Fatal("oversized store was accepted")
		}
	})
}

func TestAssociationValidationHelpers(t *testing.T) {
	for _, value := range []string{"", strings.Repeat("x", 257), "value\n", "value\x7f"} {
		if validIdentifier(value) {
			t.Fatalf("invalid identifier accepted: %q", value)
		}
	}
	if !validIdentifier("value") {
		t.Fatal("valid identifier rejected")
	}
	if _, err := validObservations([]Observation{{PaneID: " ", NativeSessionID: "session"}}); err == nil {
		t.Fatal("normalized-empty pane was accepted")
	}
	if _, err := validObservations([]Observation{{PaneID: "pane", NativeSessionID: " "}}); err == nil {
		t.Fatal("normalized-empty session was accepted")
	}
	left := map[string]Association{"pane": {PaneID: "pane", NativeSessionID: "one", ProfileID: "personal"}}
	right := map[string]Association{"pane": {PaneID: "pane", NativeSessionID: "two", ProfileID: "personal"}}
	if sameAssociations(left, right) {
		t.Fatal("different associations compared equal")
	}
}

func TestAssociationWriteFailurePreservesMemoryAndRetries(t *testing.T) {
	configHome, stateDir := configuredCustomProfiles(t)
	resolver := NewResolver(configHome, nil, WithAssociationStore(stateDir))
	if err := os.RemoveAll(stateDir); err != nil {
		t.Fatal(err)
	}
	if err := resolver.Reconcile(nil); err != nil {
		t.Fatal(err)
	}
	resolver.Remember("pane", "personal")
	observed := []Observation{{PaneID: "pane", NativeSessionID: "session"}}
	if err := os.WriteFile(stateDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := resolver.Reconcile(observed); err == nil {
		t.Fatal("write through file path succeeded")
	}
	if got := resolver.ResolvePane("pane", "copilot"); got != "personal" {
		t.Fatalf("pending association lost after write failure: %q", got)
	}
	if err := os.Remove(stateDir); err != nil {
		t.Fatal(err)
	}
	if err := resolver.Reconcile(observed); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	if got := resolver.ResolvePane("pane", "copilot"); got != "personal" {
		t.Fatalf("retried association = %q", got)
	}
}

type failingAssociationFile struct {
	name      string
	fail      string
	closeSeen bool
}

func (f *failingAssociationFile) Name() string { return f.name }
func (f *failingAssociationFile) Chmod(os.FileMode) error {
	if f.fail == "chmod" {
		return errors.New("chmod")
	}
	return nil
}
func (f *failingAssociationFile) Write(data []byte) (int, error) {
	if f.fail == "write" {
		return 0, errors.New("write")
	}
	return len(data), nil
}
func (f *failingAssociationFile) Sync() error {
	if f.fail == "sync" {
		return errors.New("sync")
	}
	return nil
}
func (f *failingAssociationFile) Close() error {
	f.closeSeen = true
	if f.fail == "close" {
		return errors.New("close")
	}
	return nil
}

type failingAssociationDirectory struct{ fail string }

func (d failingAssociationDirectory) Sync() error {
	if d.fail == "directory sync" {
		return errors.New("directory sync")
	}
	return nil
}
func (d failingAssociationDirectory) Close() error {
	if d.fail == "directory close" {
		return errors.New("directory close")
	}
	return nil
}

func TestAssociationAtomicWriterPropagatesEveryBoundaryFailure(t *testing.T) {
	directory := t.TempDir()
	info, err := os.Lstat(directory)
	if err != nil {
		t.Fatal(err)
	}
	associations := map[string]Association{
		"two": {PaneID: "two", NativeSessionID: "session-two", ProfileID: "emu"},
		"one": {PaneID: "one", NativeSessionID: "session-one", ProfileID: "personal"},
	}
	base := func() associationStoreIO {
		return associationStoreIO{
			lstat:    func(string) (os.FileInfo, error) { return info, nil },
			mkdirAll: func(string, os.FileMode) error { return nil },
			chmod:    func(string, os.FileMode) error { return nil },
			marshal:  json.Marshal,
			createTemp: func(string, string) (associationFile, error) {
				return &failingAssociationFile{name: filepath.Join(directory, "temporary")}, nil
			},
			rename: func(string, string) error { return nil },
			openDirectory: func(string) (associationDirectory, error) {
				return failingAssociationDirectory{}, nil
			},
		}
	}
	tests := []struct {
		name   string
		mutate func(*associationStoreIO)
	}{
		{name: "lstat", mutate: func(ops *associationStoreIO) {
			ops.lstat = func(string) (os.FileInfo, error) { return nil, errors.New("lstat") }
		}},
		{name: "mkdir", mutate: func(ops *associationStoreIO) {
			ops.lstat = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
			ops.mkdirAll = func(string, os.FileMode) error { return errors.New("mkdir") }
		}},
		{name: "directory chmod", mutate: func(ops *associationStoreIO) {
			ops.chmod = func(string, os.FileMode) error { return errors.New("chmod") }
		}},
		{name: "marshal", mutate: func(ops *associationStoreIO) {
			ops.marshal = func(any) ([]byte, error) { return nil, errors.New("marshal") }
		}},
		{name: "create temp", mutate: func(ops *associationStoreIO) {
			ops.createTemp = func(string, string) (associationFile, error) { return nil, errors.New("create") }
		}},
		{name: "temp chmod", mutate: func(ops *associationStoreIO) { ops.createTemp = failingAssociationTemp(directory, "chmod") }},
		{name: "temp write", mutate: func(ops *associationStoreIO) { ops.createTemp = failingAssociationTemp(directory, "write") }},
		{name: "temp sync", mutate: func(ops *associationStoreIO) { ops.createTemp = failingAssociationTemp(directory, "sync") }},
		{name: "temp close", mutate: func(ops *associationStoreIO) { ops.createTemp = failingAssociationTemp(directory, "close") }},
		{name: "rename", mutate: func(ops *associationStoreIO) { ops.rename = func(string, string) error { return errors.New("rename") } }},
		{name: "open directory", mutate: func(ops *associationStoreIO) {
			ops.openDirectory = func(string) (associationDirectory, error) { return nil, errors.New("open") }
		}},
		{name: "directory sync", mutate: func(ops *associationStoreIO) {
			ops.openDirectory = func(string) (associationDirectory, error) {
				return failingAssociationDirectory{fail: "directory sync"}, nil
			}
		}},
		{name: "directory close", mutate: func(ops *associationStoreIO) {
			ops.openDirectory = func(string) (associationDirectory, error) {
				return failingAssociationDirectory{fail: "directory close"}, nil
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ops := base()
			test.mutate(&ops)
			if err := writeAssociationStoreWith(ops, directory, associations); err == nil {
				t.Fatal("boundary failure was ignored")
			}
		})
	}
}

func failingAssociationTemp(directory, fail string) func(string, string) (associationFile, error) {
	return func(string, string) (associationFile, error) {
		return &failingAssociationFile{name: filepath.Join(directory, "temporary"), fail: fail}, nil
	}
}

func configuredCustomProfiles(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	stateDir := filepath.Join(root, "state")
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(filepath.Join(configHome, "herdr"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"personal", "emu"} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	ini := "[config]\nreplace_profiles = true\n[profiles]\npersonal = Personal\nemu = EMU\n"
	if err := os.WriteFile(filepath.Join(configHome, "herdr", "agent-profiles.ini"), []byte(ini), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	return configHome, stateDir
}
