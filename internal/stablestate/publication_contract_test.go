package stablestate_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/stablestate"
)

const stablePublicationJSON = `{
  "owner": "herdr-mobile-relay-stable-setup-v1",
  "stage": "new"
}
`

func TestStableStatePublicationJSONAndInput(t *testing.T) {
	dir := stableFixtureDir(t)
	target := filepath.Join(dir, "state.json")
	state := map[string]any{
		"z":      true,
		"owner":  stablestate.Owner,
		"a":      "<>&\"\n\t\\\u2028",
		"nested": map[string]any{"z": nil, "a": []any{float64(2), true, "inner"}},
	}
	if err := stablestate.Write(target, state); err != nil {
		t.Fatal(err)
	}
	const want = `{
  "a": "\u003c\u003e\u0026\"\n\t\\\u2028",
  "nested": {
    "a": [
      2,
      true,
      "inner"
    ],
    "z": null
  },
  "owner": "herdr-mobile-relay-stable-setup-v1",
  "z": true
}
`
	assertStableFile(t, target, want, 0o600)
	// Independent nested values, not two aliases to the input's containers.
	wantState := map[string]any{
		"a":      "<>&\"\n\t\\\u2028",
		"nested": map[string]any{"a": []any{float64(2), true, "inner"}, "z": nil},
		"owner":  "herdr-mobile-relay-stable-setup-v1",
		"z":      true,
	}
	if !reflect.DeepEqual(state, wantState) {
		t.Fatalf("input changed: %#v, want %#v", state, wantState)
	}
	assertStableEntries(t, dir, "state.json")
}

func TestStableStatePublicationDirectoriesAndReplacement(t *testing.T) {
	root := stableFixtureDir(t)
	newDir := filepath.Join(root, "new")
	stableNormalizeOnCleanup(t, newDir)
	if err := stablestate.Write(filepath.Join(newDir, "state.json"), stableNewState()); err != nil {
		t.Fatal(err)
	}
	assertStableMode(t, newDir, 0o700)
	assertStableFile(t, filepath.Join(newDir, "state.json"), stablePublicationJSON, 0o600)
	assertStableEntries(t, newDir, "state.json")

	existing := stableChildDir(t, root, "existing", 0o750)
	target := filepath.Join(existing, "state.json")
	stableFixtureFile(t, target, "old target", 0o644)
	if err := stablestate.Write(target, stableNewState()); err != nil {
		t.Fatal(err)
	}
	assertStableMode(t, existing, 0o700)
	assertStableFile(t, target, stablePublicationJSON, 0o600)
	assertStableEntries(t, existing, "state.json")
}

func TestStableStatePublicationOwnerBeforeFilesystemAndMarshal(t *testing.T) {
	cases := []struct {
		name  string
		state map[string]any
	}{
		{"nil", nil},
		{"missing", map[string]any{"bad": make(chan int)}},
		{"wrong-type", map[string]any{"owner": 1, "bad": make(chan int)}},
		{"wrong-string", map[string]any{"owner": "other", "bad": make(chan int)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := stableFixtureDir(t)
			newDir := filepath.Join(root, "absent")
			stableNormalizeOnCleanup(t, newDir)
			assertStableOwnerError(t, stablestate.Write(filepath.Join(newDir, "state.json"), tc.state))
			if _, err := os.Lstat(newDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rejected owner created directory or stat failed: %v", err)
			}
			assertStableEntries(t, root)
			existing := stableChildDir(t, root, "existing", 0o750)
			target := filepath.Join(existing, "state.json")
			stableFixtureFile(t, target, "old target", 0o644)
			assertStableOwnerError(t, stablestate.Write(target, tc.state))
			assertStableMode(t, existing, 0o750)
			assertStableFile(t, target, "old target", 0o644)
			assertStableEntries(t, existing, "state.json")
			assertStableEntries(t, root, "existing")
		})
	}
}

func TestStableStatePublicationDirectoryBeforeMarshal(t *testing.T) {
	root := stableFixtureDir(t)
	bad := map[string]any{"owner": stablestate.Owner, "bad": make(chan int)}
	newDir := filepath.Join(root, "new")
	stableNormalizeOnCleanup(t, newDir)
	err := stablestate.Write(filepath.Join(newDir, "state.json"), bad)
	var marshalErr *json.UnsupportedTypeError
	if !errors.As(err, &marshalErr) {
		t.Fatalf("write error = %v, want unsupported JSON type", err)
	}
	assertStableMode(t, newDir, 0o700)
	assertStableEntries(t, newDir)

	existing := stableChildDir(t, root, "existing", 0o750)
	target := filepath.Join(existing, "state.json")
	stableFixtureFile(t, target, "old target", 0o644)
	err = stablestate.Write(target, bad)
	if !errors.As(err, &marshalErr) {
		t.Fatalf("write error = %v, want unsupported JSON type", err)
	}
	assertStableMode(t, existing, 0o700)
	assertStableFile(t, target, "old target", 0o644)
	assertStableEntries(t, existing, "state.json")

	blocker := filepath.Join(root, "blocker")
	stableFixtureFile(t, blocker, "not a directory", 0o600)
	blockedTarget := filepath.Join(blocker, "state.json")
	err = stablestate.Write(blockedTarget, bad)
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) || pathErr.Op != "mkdir" || errors.As(err, &marshalErr) {
		t.Fatalf("mkdir-before-marshal error = %v", err)
	}
	assertStableOwnerError(t, stablestate.Write(blockedTarget, map[string]any{"owner": "other", "bad": make(chan int)}))
	assertStableFile(t, blocker, "not a directory", 0o600)
	assertStableEntries(t, root, "blocker", "existing", "new")
}

func TestStableStatePublicationRealRenameFailureDirectory(t *testing.T) {
	root := stableFixtureDir(t)
	target := stableChildDir(t, root, "state.json", 0o700)
	sentinel := filepath.Join(target, "sentinel")
	stableFixtureFile(t, sentinel, "keep sentinel", 0o600)
	err := stablestate.Write(target, stableNewState())
	var linkErr *os.LinkError
	if !errors.As(err, &linkErr) || linkErr.Op != "rename" || linkErr.New != target {
		t.Fatalf("write error = %v, want actual rename failure", err)
	}
	assertStableMode(t, target, 0o700)
	assertStableFile(t, sentinel, "keep sentinel", 0o600)
	assertStableEntries(t, target, "sentinel")
	assertStableEntries(t, root, "state.json")
}

func TestStableStatePublicationNoncanonicalRawTarget(t *testing.T) {
	root := stableFixtureDir(t)
	lexical := stableChildDir(t, root, "lexical", 0o750)
	physical := stableChildDir(t, root, "physical", 0o750)
	child := stableChildDir(t, physical, "child", 0o700)
	if err := os.Symlink(child, filepath.Join(lexical, "link")); err != nil {
		t.Fatal(err)
	}
	lexicalTarget := filepath.Join(lexical, "state.json")
	physicalTarget := filepath.Join(physical, "state.json")
	stableFixtureFile(t, lexicalTarget, "lexical sentinel", 0o644)
	stableFixtureFile(t, physicalTarget, "physical sentinel", 0o644)
	// Join would erase the symlink/.. distinction before Write sees it.
	rawTarget := lexical + string(os.PathSeparator) + "link" + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "state.json"
	resolved, err := filepath.EvalSymlinks(rawTarget)
	if err != nil {
		t.Fatal(err)
	}
	wantResolved, err := filepath.EvalSymlinks(physicalTarget)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != wantResolved || filepath.Dir(rawTarget) != lexical || rawTarget == lexicalTarget {
		t.Fatalf("fixture resolution: raw %q -> %q, want %q; directory %q", rawTarget, resolved, wantResolved, filepath.Dir(rawTarget))
	}
	if err := stablestate.Write(rawTarget, stableNewState()); err != nil {
		t.Fatal(err)
	}
	assertStableMode(t, lexical, 0o700)
	assertStableMode(t, physical, 0o750)
	assertStableFile(t, lexicalTarget, "lexical sentinel", 0o644)
	assertStableFile(t, physicalTarget, stablePublicationJSON, 0o600)
	assertStableEntries(t, lexical, "link", "state.json")
	assertStableEntries(t, physical, "child", "state.json")
	assertStableEntries(t, child)
}

func TestStableStatePublicationEntryEnumeration(t *testing.T) {
	dir := stableFixtureDir(t)
	if _, err := stableFixtureEntries(filepath.Join(dir, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing directory error = %v, want not exist", err)
	}
	assertStableEntries(t, dir)
	stableFixtureFile(t, filepath.Join(dir, "state.json"), "fixture", 0o600)
	stableFixtureFile(t, filepath.Join(dir, ".state.json.synthetic-leftover"), "leftover", 0o600)
	names, err := stableFixtureEntries(dir)
	if err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(names, []string{"state.json"}) {
		t.Fatal("enumeration hid the actual synthetic leftover")
	}
	assertStableEntries(t, dir, ".state.json.synthetic-leftover", "state.json")
}

func stableNewState() map[string]any {
	return map[string]any{"owner": stablestate.Owner, "stage": "new"}
}

func stableFixtureDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "herdr-stablestate-contract-test-*")
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
	assertStableMode(t, dir, 0o700)
	return dir
}

func stableNormalizeOnCleanup(t *testing.T, dir string) {
	t.Helper()
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o700); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("normalize fixture directory: %v", err)
		}
	})
}

func stableChildDir(t *testing.T, parent, name string, mode os.FileMode) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	stableNormalizeOnCleanup(t, dir)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, mode); err != nil {
		t.Fatal(err)
	}
	assertStableMode(t, dir, mode)
	return dir
}

func stableFixtureFile(t *testing.T, path, data string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	assertStableFile(t, path, data, mode)
}

func assertStableMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %o, want %o", path, got, want)
	}
}

func assertStableFile(t *testing.T, path, want string, mode os.FileMode) {
	t.Helper()
	assertStableMode(t, path, mode)
	data, err := os.ReadFile(path)
	if err != nil || string(data) != want {
		t.Fatalf("%s bytes = %q, error %v; want %q", path, data, err, want)
	}
}

func assertStableOwnerError(t *testing.T, err error) {
	t.Helper()
	if err == nil || err.Error() != "refusing to write unowned stable state" {
		t.Fatalf("owner error = %v", err)
	}
}

func stableFixtureEntries(dir string) ([]string, error) {
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

func assertStableEntries(t *testing.T, dir string, want ...string) {
	t.Helper()
	names, err := stableFixtureEntries(dir)
	if err != nil {
		t.Fatalf("list fixture directory: %v", err)
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("fixture entries = %v, want %v", names, want)
	}
}
