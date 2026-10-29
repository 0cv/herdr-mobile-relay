//go:build !herdr_sqlite_logical_test || herdr_installed_hosted_test || herdr_tailscale_test || herdr_tailscale_cli_fixture_binary

package conversation

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestMain(m *testing.M) {
	if runtime.GOOS == "darwin" {
		if tempDir, err := filepath.EvalSymlinks("/tmp"); err == nil {
			_ = os.Setenv("TMPDIR", tempDir)
		}
	}
	os.Exit(m.Run())
}
