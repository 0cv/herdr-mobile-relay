package support

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteAndLoadPrivateSnapshot(t *testing.T) {
	dir := t.TempDir()
	snapshot := Snapshot{
		Version:   "1.2.3",
		Revision:  "abc123",
		Protocol:  2,
		Readiness: "ready",
		Inventory: map[string]any{"state": "ready"},
		Components: map[string]string{
			"http": "running",
		},
		RecentErrors: []string{"safe error"},
	}
	if err := Write(dir, snapshot); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("support mode = %o, want 600", info.Mode().Perm())
	}
	loaded, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Version != snapshot.Version || loaded.Protocol != snapshot.Protocol ||
		loaded.Components["http"] != "running" || loaded.GeneratedAt == "" {
		t.Fatalf("loaded snapshot = %+v", loaded)
	}
}

func TestWriteCreatesPrivateDirectoryAndStampsTime(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runtime")
	before := time.Now().UTC().Truncate(time.Second)
	if err := Write(dir, Snapshot{GeneratedAt: "original"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode = %o, want 700", info.Mode().Perm())
	}
	loaded, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	generated, err := time.Parse(time.RFC3339, loaded.GeneratedAt)
	if err != nil || generated.Before(before) {
		t.Fatalf("generated_at = %q, error %v, want a time not before %v", loaded.GeneratedAt, err, before)
	}
}

func TestWriteReplacesExistingSnapshotWithoutLeftovers(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(Path(dir), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Write(dir, Snapshot{Version: "new"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("support mode = %o, want 600", info.Mode().Perm())
	}
	loaded, err := Load(dir)
	if err != nil || loaded.Version != "new" {
		t.Fatalf("loaded = %+v, error %v", loaded, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("directory entries = %v, want only the snapshot", entries)
	}
}
