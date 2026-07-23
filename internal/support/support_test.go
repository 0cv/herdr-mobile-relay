package support

import (
	"os"
	"testing"
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
