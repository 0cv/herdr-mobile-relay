package blackbox

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestMain(m *testing.M) {
	if runtime.GOOS == "darwin" {
		if tempDir, err := filepath.EvalSymlinks(os.TempDir()); err == nil {
			_ = os.Setenv("TMPDIR", tempDir)
		}
	}
	os.Exit(m.Run())
}
