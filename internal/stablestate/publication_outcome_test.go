package stablestate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/atomicfile"
)

const stableOutcomeJSON = `{
  "owner": "herdr-mobile-relay-stable-setup-v1",
  "stage": "new"
}
`

func TestStableStatePublicationArgumentsAndUnconfirmedError(t *testing.T) {
	dir := stableOutcomeFixtureDir(t)
	child := filepath.Join(dir, "child")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(child, 0o700); err != nil {
		t.Fatal(err)
	}
	stableOutcomeMode(t, child, 0o700)
	target := filepath.Join(dir, "state.json")
	stableOutcomeOldFile(t, target)
	if err := os.Chmod(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	stableOutcomeMode(t, dir, 0o750)
	rawTarget := dir + string(os.PathSeparator) + "child" + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "state.json"
	sentinel := errors.New("synthetic unconfirmed publication error")
	calls := 0
	err := write(rawTarget, map[string]any{"stage": "new", "owner": Owner}, func(directory, path, pattern string, data []byte, mode os.FileMode) (atomicfile.Outcome, error) {
		calls++
		if directory != dir || path != rawTarget || path == target || pattern != ".state.json." || string(data) != stableOutcomeJSON || mode != 0o600 {
			t.Fatalf("publisher arguments = %q, %q, %q, %q, %o", directory, path, pattern, data, mode)
		}
		stableOutcomeMode(t, dir, 0o700)
		return atomicfile.RenameNotConfirmed, sentinel
	})
	if calls != 1 || err != sentinel {
		t.Fatalf("calls = %d, error = %v; want identical sentinel %v", calls, err, sentinel)
	}
	stableOutcomeFile(t, target, "old target", 0o644)
	assertStableOutcomeEntries(t, dir, "child", "state.json")
}

func TestStableStatePublicationConfirmedErrorsRemainErrors(t *testing.T) {
	// Labels describe synthetic adapter outcomes AFTER verified real replacement,
	// not induced kernel directory-open/sync failures. Frozen helper tests inject
	// those algorithm stages independently.
	for _, stage := range []string{"directory-open", "directory-sync"} {
		t.Run(stage, func(t *testing.T) {
			dir := stableOutcomeFixtureDir(t)
			target := filepath.Join(dir, "state.json")
			stableOutcomeOldFile(t, target)
			sentinel := errors.New("synthetic " + stage + " error")
			calls := 0
			err := write(target, map[string]any{"owner": Owner, "stage": "new"}, func(directory, path, pattern string, data []byte, mode os.FileMode) (atomicfile.Outcome, error) {
				calls++
				outcome, err := atomicfile.Replace(directory, path, pattern, data, mode)
				if outcome != atomicfile.Renamed || err != nil {
					t.Fatalf("actual replacement = %v, %v", outcome, err)
				}
				stableOutcomeFile(t, target, stableOutcomeJSON, 0o600)
				assertStableOutcomeEntries(t, dir, "state.json")
				return atomicfile.Renamed, sentinel
			})
			if calls != 1 || err != sentinel {
				t.Fatalf("calls = %d, error = %v; want identical sentinel %v", calls, err, sentinel)
			}
			stableOutcomeFile(t, target, stableOutcomeJSON, 0o600)
			assertStableOutcomeEntries(t, dir, "state.json")
		})
	}
}

func TestStableStatePublicationActualSuccess(t *testing.T) {
	dir := stableOutcomeFixtureDir(t)
	target := filepath.Join(dir, "state.json")
	stableOutcomeOldFile(t, target)
	if err := write(target, map[string]any{"owner": Owner, "stage": "new"}, atomicfile.Replace); err != nil {
		t.Fatal(err)
	}
	stableOutcomeFile(t, target, stableOutcomeJSON, 0o600)
	assertStableOutcomeEntries(t, dir, "state.json")
}

func TestStableStatePublicationDomainFailuresDoNotPublish(t *testing.T) {
	cases := []struct {
		name  string
		state map[string]any
		kind  string
	}{
		{"nil-owner", nil, "owner"},
		{"missing-owner", map[string]any{"bad": make(chan int)}, "owner"},
		{"wrong-type-owner", map[string]any{"owner": 1, "bad": make(chan int)}, "owner"},
		{"wrong-string-owner", map[string]any{"owner": "other", "bad": make(chan int)}, "owner"},
		{"mkdir-before-marshal", map[string]any{"owner": Owner, "bad": make(chan int)}, "mkdir"},
		{"marshal-after-directory", map[string]any{"owner": Owner, "bad": make(chan int)}, "marshal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := stableOutcomeFixtureDir(t)
			dir := filepath.Join(root, "parent")
			if tc.kind == "mkdir" {
				stableOutcomeOldFile(t, dir)
			} else {
				t.Cleanup(func() {
					if err := os.Chmod(dir, 0o700); err != nil && !errors.Is(err, os.ErrNotExist) {
						t.Errorf("normalize fixture directory: %v", err)
					}
				})
			}
			called := false
			err := write(filepath.Join(dir, "state.json"), tc.state, func(string, string, string, []byte, os.FileMode) (atomicfile.Outcome, error) {
				called = true
				return atomicfile.RenameNotConfirmed, errors.New("unexpected publisher call")
			})
			if called {
				t.Fatal("publisher called after domain failure")
			}
			switch tc.kind {
			case "owner":
				if err == nil || err.Error() != "refusing to write unowned stable state" {
					t.Fatalf("owner error = %v", err)
				}
				assertStableOutcomeEntries(t, root)
			case "mkdir":
				var pathErr *os.PathError
				if !errors.As(err, &pathErr) || pathErr.Op != "mkdir" {
					t.Fatalf("mkdir error = %v", err)
				}
				stableOutcomeFile(t, dir, "old target", 0o644)
				assertStableOutcomeEntries(t, root, "parent")
			case "marshal":
				var marshalErr *json.UnsupportedTypeError
				if !errors.As(err, &marshalErr) {
					t.Fatalf("marshal error = %v", err)
				}
				stableOutcomeMode(t, dir, 0o700)
				assertStableOutcomeEntries(t, dir)
				assertStableOutcomeEntries(t, root, "parent")
			}
		})
	}
}

func TestStableStatePublicationOutcomeEntryEnumeration(t *testing.T) {
	dir := stableOutcomeFixtureDir(t)
	if _, err := stableOutcomeEntries(filepath.Join(dir, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing directory error = %v, want not exist", err)
	}
	assertStableOutcomeEntries(t, dir)
	stableOutcomeOldFile(t, filepath.Join(dir, "state.json"))
	stableOutcomeOldFile(t, filepath.Join(dir, ".state.json.synthetic-leftover"))
	assertStableOutcomeEntries(t, dir, ".state.json.synthetic-leftover", "state.json")
}

func stableOutcomeFixtureDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "herdr-stablestate-outcome-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Errorf("normalize fixture root: %v", err)
		}
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove fixture root: %v", err)
		}
	})
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stableOutcomeMode(t, dir, 0o700)
	return dir
}

func stableOutcomeOldFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("old target"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	stableOutcomeFile(t, path, "old target", 0o644)
}

func stableOutcomeMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %o, want %o", path, got, want)
	}
}

func stableOutcomeFile(t *testing.T, path, want string, mode os.FileMode) {
	t.Helper()
	stableOutcomeMode(t, path, mode)
	data, err := os.ReadFile(path)
	if err != nil || string(data) != want {
		t.Fatalf("%s bytes = %q, error %v; want %q", path, data, err, want)
	}
}

func stableOutcomeEntries(dir string) ([]string, error) {
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

func assertStableOutcomeEntries(t *testing.T, dir string, want ...string) {
	t.Helper()
	names, err := stableOutcomeEntries(dir)
	if err != nil {
		t.Fatalf("list fixture directory: %v", err)
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("fixture entries = %v, want %v", names, want)
	}
}
