package support_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/support"
)

func TestSupportPublicationJSON(t *testing.T) {
	dir := supportFixtureDir(t)
	snapshot := support.Snapshot{
		GeneratedAt:  "original",
		Version:      "1.2.3",
		Revision:     "abc",
		Protocol:     2,
		Readiness:    "ready",
		Inventory:    map[string]any{"state": "ready"},
		Components:   map[string]string{"http": "running"},
		RecentErrors: []string{"safe error"},
	}
	before := time.Now().UTC().Truncate(time.Second)
	if err := support.Write(dir, snapshot); err != nil {
		t.Fatal(err)
	}
	after := time.Now().UTC().Truncate(time.Second)
	data, err := os.ReadFile(support.Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	var loaded support.Snapshot
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatal(err)
	}
	generated, err := time.Parse(time.RFC3339, loaded.GeneratedAt)
	if err != nil || generated.Location() != time.UTC || generated.Format(time.RFC3339) != loaded.GeneratedAt || generated.Before(before) || generated.After(after) {
		t.Fatalf("generated_at = %q, parsed %v, error %v, bounds [%v, %v]", loaded.GeneratedAt, generated, err, before, after)
	}
	want := fmt.Sprintf(`{
  "generated_at": %q,
  "version": "1.2.3",
  "revision": "abc",
  "protocol": 2,
  "readiness": "ready",
  "inventory": {
    "state": "ready"
  },
  "components": {
    "http": "running"
  },
  "activity_failures": 0,
  "topology_retries": 0,
  "poll_failures": 0,
  "recent_errors": [
    "safe error"
  ]
}`, loaded.GeneratedAt)
	if string(data) != want {
		t.Fatalf("snapshot bytes = %q, want %q", data, want)
	}
	if snapshot.GeneratedAt != "original" || snapshot.Inventory["state"] != "ready" || snapshot.Components["http"] != "running" || snapshot.RecentErrors[0] != "safe error" {
		t.Fatalf("input snapshot changed: %+v", snapshot)
	}
}

func TestSupportPublicationDirectoriesAndReplacement(t *testing.T) {
	root := supportFixtureDir(t)
	controlDir := filepath.Join(root, "control")
	supportNormalizeOnCleanup(t, controlDir)
	if err := os.Mkdir(controlDir, 0o700); err != nil {
		t.Fatal(err)
	}
	controlInfo, err := os.Stat(controlDir)
	if err != nil {
		t.Fatal(err)
	}
	newDir := filepath.Join(root, "new")
	supportNormalizeOnCleanup(t, newDir)
	if err := support.Write(newDir, support.Snapshot{}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(newDir)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode().Perm(), controlInfo.Mode().Perm(); got != want {
		t.Fatalf("new directory mode = %o, control mode = %o", got, want)
	}
	if err := os.Chmod(controlDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(newDir, 0o700); err != nil {
		t.Fatal(err)
	}

	existing := filepath.Join(root, "existing")
	if err := os.Mkdir(existing, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(existing, 0o750); err != nil {
		t.Fatal(err)
	}
	target := support.Path(existing)
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := support.Write(existing, support.Snapshot{Version: "new"}); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(existing)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o750 {
		t.Fatalf("existing directory mode = %o, want 750", got)
	}
	info, err = os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("replacement mode = %o, want 600", got)
	}
	loaded, err := support.Load(existing)
	if err != nil || loaded.Version != "new" {
		t.Fatalf("replacement = %+v, error %v", loaded, err)
	}
	assertSupportEntries(t, existing, "support-state.json")
	assertSupportEntries(t, newDir, "support-state.json")
}

func TestSupportPublicationMkdirBeforeMarshal(t *testing.T) {
	root := supportFixtureDir(t)
	bad := support.Snapshot{Scheduler: make(chan int)}
	newDir := filepath.Join(root, "created-before-marshal")
	supportNormalizeOnCleanup(t, newDir)
	err := support.Write(newDir, bad)
	var marshalErr *json.UnsupportedTypeError
	if !errors.As(err, &marshalErr) {
		t.Fatalf("write error = %v, want unsupported type", err)
	}
	if info, err := os.Stat(newDir); err != nil || !info.IsDir() {
		t.Fatalf("directory after marshal error = %v, error %v", info, err)
	}
	if err := os.Chmod(newDir, 0o700); err != nil {
		t.Fatal(err)
	}
	assertSupportEntries(t, newDir)

	existing := filepath.Join(root, "existing")
	if err := os.Mkdir(existing, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(existing, 0o700); err != nil {
		t.Fatal(err)
	}
	target := support.Path(existing)
	if err := os.WriteFile(target, []byte("old regular target"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatal(err)
	}
	err = support.Write(existing, bad)
	if !errors.As(err, &marshalErr) {
		t.Fatalf("write error = %v, want unsupported type", err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "old regular target" {
		t.Fatalf("old target = %q, error %v", data, err)
	}
	assertSupportEntries(t, existing, "support-state.json")

	blocker := filepath.Join(root, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blocker, 0o600); err != nil {
		t.Fatal(err)
	}
	err = support.Write(filepath.Join(blocker, "child"), bad)
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) || pathErr.Op != "mkdir" || errors.As(err, &marshalErr) {
		t.Fatalf("mkdir-before-marshal error = %v", err)
	}
	data, err = os.ReadFile(blocker)
	if err != nil || string(data) != "not a directory" {
		t.Fatalf("blocker = %q, error %v", data, err)
	}
}

func TestSupportPublicationRealRenameFailureDirectory(t *testing.T) {
	dir := supportFixtureDir(t)
	target := support.Path(dir)
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(target, "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sentinel, 0o600); err != nil {
		t.Fatal(err)
	}
	err := support.Write(dir, support.Snapshot{Version: "new"})
	var linkErr *os.LinkError
	if !errors.As(err, &linkErr) || linkErr.Op != "rename" {
		t.Fatalf("write error = %v, want real rename failure", err)
	}
	data, err := os.ReadFile(sentinel)
	if err != nil || string(data) != "keep" {
		t.Fatalf("sentinel = %q, error %v", data, err)
	}
	assertSupportEntries(t, dir, "support-state.json")
}

func TestSupportPublicationEntryEnumeration(t *testing.T) {
	dir := supportFixtureDir(t)
	if _, err := supportFixtureEntries(filepath.Join(dir, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing directory error = %v, want not exist", err)
	}
	for _, name := range []string{"support-state.json", ".support-state.synthetic.tmp"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	assertSupportEntries(t, dir, ".support-state.synthetic.tmp", "support-state.json")
}

func supportFixtureDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "herdr-support-contract-test-*")
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

func supportNormalizeOnCleanup(t *testing.T, dir string) {
	t.Helper()
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o700); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("normalize fixture directory for cleanup: %v", err)
		}
	})
}

func supportFixtureEntries(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names, nil
}

func assertSupportEntries(t *testing.T, dir string, want ...string) {
	t.Helper()
	entries, err := supportFixtureEntries(dir)
	if err != nil {
		t.Fatalf("list fixture directory: %v", err)
	}
	if !reflect.DeepEqual(entries, want) {
		t.Fatalf("fixture entries = %v, want %v", entries, want)
	}
}
