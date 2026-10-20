package hermesprofiles

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// isolate clears the roots the package reads from the environment, so a
// developer's own HERMES_HOME cannot add profiles to a test's home directory.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("HERMES_HOME", "")
	t.Setenv("HERDR_HERMES_DATA_DIRS", "")
}

// writeProfile creates <root>/profiles/<name> as a directory.
func writeProfile(t *testing.T, root, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "profiles", name), 0o755); err != nil {
		t.Fatalf("create profile %q: %v", name, err)
	}
}

func TestValid(t *testing.T) {
	for _, name := range []string{"default", "apollo", "a", "work-2", "under_score", "a1234567890"} {
		if !Valid(name) {
			t.Errorf("Valid(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"", "-leading", "_leading", "Upper", "with space", "dot.name", "sl/ash"} {
		if Valid(name) {
			t.Errorf("Valid(%q) = true, want false", name)
		}
	}
	// NamePattern is ^[a-z0-9][a-z0-9_-]{0,63}$: one leading character plus up
	// to 63 more, so 64 characters is the longest accepted name and 65 must be
	// refused or the picker could offer a name hermes rejects.
	if !Valid("a" + repeat("b", 63)) {
		t.Error("Valid(64 chars) = false, want true")
	}
	if Valid("a" + repeat("b", 64)) {
		t.Error("Valid(65 chars) = true, want false")
	}
}

func repeat(value string, count int) string {
	out := make([]byte, 0, count)
	for range count {
		out = append(out, value...)
	}
	return string(out)
}

func TestListAlwaysOffersDefaultFirst(t *testing.T) {
	isolate(t)
	home := t.TempDir()

	got := List(home)
	if !reflect.DeepEqual(got, []string{Default}) {
		t.Fatalf("List(empty home) = %#v, want [%q]", got, Default)
	}
}

func TestListReportsSortedNamedProfiles(t *testing.T) {
	isolate(t)
	home := t.TempDir()
	root := filepath.Join(home, ".hermes")
	for _, name := range []string{"work", "apollo", "zeta"} {
		writeProfile(t, root, name)
	}
	// A file is not a profile, an invalid name cannot be handed to `-p`, and
	// Default belongs to the home itself rather than to profiles/.
	if err := os.WriteFile(filepath.Join(root, "profiles", "stray"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "profiles", "Bad Name"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeProfile(t, root, Default)

	got := List(home)
	want := []string{Default, "apollo", "work", "zeta"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("List() = %#v, want %#v", got, want)
	}
}

func TestListScansRelocatedHermesHome(t *testing.T) {
	isolate(t)
	home := t.TempDir()
	relocated := filepath.Join(t.TempDir(), "hermes-home")
	t.Setenv("HERMES_HOME", relocated)
	writeProfile(t, relocated, "apollo")

	got := List(home)
	want := []string{Default, "apollo"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("List() = %#v, want %#v", got, want)
	}
}

func TestListFollowsSymlinkedProfile(t *testing.T) {
	isolate(t)
	home := t.TempDir()
	root := filepath.Join(home, ".hermes")
	if err := os.MkdirAll(filepath.Join(root, "profiles"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "apollo")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "profiles", "apollo")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	got := List(home)
	want := []string{Default, "apollo"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("List() = %#v, want %#v", got, want)
	}
	if !Exists(home, "apollo") {
		t.Error("Exists(symlinked profile) = false, want true")
	}
}

func TestExists(t *testing.T) {
	isolate(t)
	home := t.TempDir()
	root := filepath.Join(home, ".hermes")
	writeProfile(t, root, "apollo")
	if err := os.WriteFile(filepath.Join(root, "profiles", "stray"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if !Exists(home, Default) {
		t.Error("Exists(default) = false, want true even with no profiles directory")
	}
	if !Exists(filepath.Join(t.TempDir(), "nowhere"), Default) {
		t.Error("Exists(default) on an empty home = false, want true")
	}
	if !Exists(home, "apollo") {
		t.Error("Exists(apollo) = false, want true")
	}
	if Exists(home, "missing") {
		t.Error("Exists(missing) = true, want false")
	}
	if Exists(home, "stray") {
		t.Error("Exists(stray file) = true, want false")
	}
	if Exists(home, "Bad Name") {
		t.Error("Exists(invalid name) = true, want false")
	}
}
