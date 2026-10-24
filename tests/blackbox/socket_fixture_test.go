package blackbox

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "s-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	return dir
}

func TestBlackboxSocketDirWithLongTestName(t *testing.T) {
	t.Run(strings.Repeat("long-test-name-", 8), func(t *testing.T) {
		socket := filepath.Join(testSocketDir(t), "herdr.sock")
		listener := startInventorySocket(t, socket, "{}")
		t.Cleanup(func() { _ = listener.Close() })
		canonical, err := filepath.EvalSymlinks(socket)
		if err != nil {
			t.Fatal(err)
		}
		if len(canonical) > 103 {
			t.Fatalf("socket path is %d bytes, want at most 103: %s", len(canonical), canonical)
		}
	})
}

func TestBlackboxSocketDirUsesSelectedTempAndCleansUp(t *testing.T) {
	selected := testSocketDir(t)
	t.Setenv("TMPDIR", selected)
	var directory string
	t.Run(strings.Repeat("long-test-name-", 8), func(t *testing.T) {
		directory = testSocketDir(t)
		if filepath.Dir(directory) != selected || !strings.HasPrefix(filepath.Base(directory), "s-") {
			t.Fatalf("socket directory %q is not short and under %q", directory, selected)
		}
		info, err := os.Lstat(directory)
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("socket directory mode = %s", info.Mode())
		}
		socket := filepath.Join(directory, "herdr.sock")
		listener, err := net.Listen("unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = listener.Close() })
		canonical, err := filepath.EvalSymlinks(socket)
		if err != nil {
			t.Fatal(err)
		}
		if len(canonical) > 103 {
			t.Fatalf("socket path is %d bytes, want at most 103: %s", len(canonical), canonical)
		}
	})
	if _, err := os.Lstat(directory); !os.IsNotExist(err) {
		t.Fatalf("socket directory was not cleaned up: %v", err)
	}
}
