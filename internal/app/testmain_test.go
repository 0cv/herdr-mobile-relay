//go:build !herdr_installed_inert_test || !herdr_installed_hosted_test

package app

import (
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

// Leave room for testing's generated directory and Unix socket basename.
func privateShortAppTemp(path string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || len(path) > 40 {
		return false
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return false
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

func TestMain(m *testing.M) {
	if runtime.GOOS == "darwin" && !privateShortAppTemp(os.Getenv("TMPDIR")) {
		if tempDir, err := filepath.EvalSymlinks("/tmp"); err == nil {
			_ = os.Setenv("TMPDIR", tempDir)
		}
	}
	os.Exit(m.Run())
}
