package support

import (
	"bytes"
	"errors"
	"os"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/atomicfile"
)

func TestSupportPublicationIgnoresConfirmedPostRenameError(t *testing.T) {
	dir := supportOutcomeFixtureDir(t)
	target := Path(dir)
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatal(err)
	}
	postErr := errors.New("synthetic directory error")
	called := false
	err := write(dir, Snapshot{Version: "new"}, func(directory, path, pattern string, data []byte, mode os.FileMode) (atomicfile.Outcome, error) {
		called = true
		if directory != dir || path != target || pattern != ".support-state.*.tmp" || mode != 0o600 {
			t.Fatalf("publisher arguments = %q, %q, %q, %o", directory, path, pattern, mode)
		}
		outcome, err := atomicfile.Replace(directory, path, pattern, data, mode)
		if outcome != atomicfile.Renamed || err != nil {
			t.Fatalf("actual replacement = %v, %v", outcome, err)
		}
		published, err := os.ReadFile(target)
		if err != nil || !bytes.Equal(published, data) {
			t.Fatalf("actual published bytes = %q, error %v", published, err)
		}
		return atomicfile.Renamed, postErr
	})
	if !called || err != nil {
		t.Fatalf("write called = %v, error = %v", called, err)
	}
	loaded, err := Load(dir)
	if err != nil || loaded.Version != "new" || loaded.GeneratedAt == "" {
		t.Fatalf("loaded replacement = %+v, error %v", loaded, err)
	}
	info, err := os.Stat(target)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("target mode = %v, error %v", info, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("list fixture directory: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "support-state.json" {
		t.Fatalf("fixture entries = %v, want only support-state.json", entries)
	}
}

func TestSupportPublicationReturnsUnconfirmedError(t *testing.T) {
	dir := supportOutcomeFixtureDir(t)
	target := Path(dir)
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatal(err)
	}
	preErr := errors.New("synthetic pre-rename error")
	called := false
	err := write(dir, Snapshot{Version: "new"}, func(directory, path, pattern string, data []byte, mode os.FileMode) (atomicfile.Outcome, error) {
		called = true
		return atomicfile.RenameNotConfirmed, preErr
	})
	if !called || err != preErr {
		t.Fatalf("write called = %v, error = %v; want %v", called, err, preErr)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "old" {
		t.Fatalf("old target = %q, error %v", data, err)
	}
}

func supportOutcomeFixtureDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "herdr-support-outcome-test-*")
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
