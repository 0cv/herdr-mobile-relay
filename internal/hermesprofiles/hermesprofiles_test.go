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

// writeProfile creates <root>/profiles/<name> as a live profile directory: a
// marker file inside it is what hermes' named_profile_is_live checks, so a bare
// directory would be treated as a ghost and never listed.
func writeProfile(t *testing.T, root, name string) {
	t.Helper()
	writeProfileWith(t, root, name, "config.yaml")
}

// writeProfileWith creates <root>/profiles/<name> carrying the given marker file.
func writeProfileWith(t *testing.T, root, name, marker string) {
	t.Helper()
	dir := filepath.Join(root, "profiles", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create profile %q: %v", name, err)
	}
	if marker == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, marker), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write marker for profile %q: %v", name, err)
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
	if err := os.WriteFile(filepath.Join(target, "config.yaml"), []byte("{}"), 0o644); err != nil {
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

// TestListAndExistsIgnoreMarkerlessProfile mirrors hermes' named_profile_is_live:
// a profile directory with none of the identity markers is a ghost left behind by
// an interrupted create or delete, and `-p` refuses it.
func TestListAndExistsIgnoreMarkerlessProfile(t *testing.T) {
	isolate(t)
	home := t.TempDir()
	root := filepath.Join(home, ".hermes")
	writeProfile(t, root, "apollo")
	writeProfileWith(t, root, "ghost", "")
	// Hermes checks is_file() or is_symlink(), so a directory carrying a
	// marker's name does not make a profile live.
	writeProfileWith(t, root, "dirmarker", "")
	if err := os.Mkdir(filepath.Join(root, "profiles", "dirmarker", "config.yaml"), 0o755); err != nil {
		t.Fatal(err)
	}

	if got := List(home); !reflect.DeepEqual(got, []string{Default, "apollo"}) {
		t.Fatalf("List() = %#v, want %#v", got, []string{Default, "apollo"})
	}
	if Exists(home, "ghost") {
		t.Error("Exists(ghost) = true, want false for a directory with no identity marker")
	}
	if Exists(home, "dirmarker") {
		t.Error("Exists(dirmarker) = true, want false for a marker that is a directory")
	}
}

// TestListAndExistsAcceptEveryIdentityMarker keeps the marker set in step with
// hermes: any single one of them makes a profile live.
func TestListAndExistsAcceptEveryIdentityMarker(t *testing.T) {
	for _, marker := range liveMarkers {
		t.Run("marker="+marker, func(t *testing.T) {
			isolate(t)
			home := t.TempDir()
			root := filepath.Join(home, ".hermes")
			writeProfileWith(t, root, "apollo", marker)

			if got := List(home); !reflect.DeepEqual(got, []string{Default, "apollo"}) {
				t.Fatalf("List() = %#v, want %#v", got, []string{Default, "apollo"})
			}
			if !Exists(home, "apollo") {
				t.Errorf("Exists(apollo) = false with marker %q", marker)
			}
		})
	}
}

// TestListAndExistsAcceptDanglingSymlinkMarker mirrors hermes' predicate, which
// is is_file() OR is_symlink(): a marker symlink whose target has moved away is
// still an identity claim left behind by a clone or a migration. os.Stat follows
// the link to ENOENT, so without an os.Lstat probe the relay would hide a profile
// hermes itself would start.
func TestListAndExistsAcceptDanglingSymlinkMarker(t *testing.T) {
	isolate(t)
	home := t.TempDir()
	root := filepath.Join(home, ".hermes")
	dir := filepath.Join(root, "profiles", "apollo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "config.yaml")
	if err := os.Symlink(filepath.Join(t.TempDir(), "moved-away.yaml"), marker); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// The fixture is pointless unless the link really dangles.
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("Stat(%q) resolved, want a dangling symlink", marker)
	}

	if got := List(home); !reflect.DeepEqual(got, []string{Default, "apollo"}) {
		t.Fatalf("List() = %#v, want %#v", got, []string{Default, "apollo"})
	}
	if !Exists(home, "apollo") {
		t.Error("Exists(apollo) = false, want true for a dangling marker symlink")
	}
}

// TestListAndExistsIgnoreTombstonedProfile covers .deleted/<name>: a directory
// that still carries markers is dead once hermes has moved a copy under the
// tombstone, so offering it would send the phone into a start hermes refuses.
func TestListAndExistsIgnoreTombstonedProfile(t *testing.T) {
	isolate(t)
	home := t.TempDir()
	root := filepath.Join(home, ".hermes")
	writeProfile(t, root, "apollo")
	writeProfile(t, root, "work")
	// `hermes profile delete` writes this marker file before removing the
	// directory; the directory still being here is the stale-process case.
	if err := os.MkdirAll(filepath.Join(root, "profiles", ".deleted"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "profiles", ".deleted", "work"), []byte("deleted\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := List(home); !reflect.DeepEqual(got, []string{Default, "apollo"}) {
		t.Fatalf("List() = %#v, want %#v", got, []string{Default, "apollo"})
	}
	if !Exists(home, "apollo") {
		t.Error("Exists(apollo) = false, want true for an untombstoned profile")
	}
	if Exists(home, "work") {
		t.Error("Exists(work) = true, want false for a tombstoned profile")
	}
}
