package app

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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

func testPiHerdrSocket(t *testing.T) string {
	t.Helper()
	if path := os.Getenv("HERDR_TEST_PI_SOCKET"); path != "" {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		relative, err := filepath.Rel(os.TempDir(), path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || info.Mode()&os.ModeSocket == 0 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
			t.Fatal("preallocated Pi fixture socket must be owned and under the selected temporary directory")
		}
		return path
	}
	path := filepath.Join(testSocketDir(t), "herdr.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return path
}

func TestSocketDirUsesSelectedTempAndCleansUp(t *testing.T) {
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
		listener, err := net.Listen("unix", filepath.Join(directory, "herdr.sock"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = listener.Close() })
	})
	if _, err := os.Lstat(directory); !os.IsNotExist(err) {
		t.Fatalf("socket directory was not cleaned up: %v", err)
	}
}
