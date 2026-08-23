package coordinator

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveCwdReturnsCanonicalSymlinkTarget(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(home, "projects", "app")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, "app-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	resolved, err := (&Lifecycle{home: home}).ResolveCwd(link)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != target {
		t.Fatalf("resolved cwd = %q, want %q", resolved, target)
	}
}
