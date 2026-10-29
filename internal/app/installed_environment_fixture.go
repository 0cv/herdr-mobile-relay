//go:build herdr_installed_inert_test

package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// IsolateInstalledFixture must precede construction. Clearing all inherited
// overrides also confines agent-root defaults to a private empty HOME.
func IsolateInstalledFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		t.Setenv(key, "")
	}
	for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_RUNTIME_DIR", "TMPDIR", "PATH"} {
		dir := filepath.Join(root, key)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv(key, dir)
	}
	// Callers are nonparallel and must join owned work before test cleanup.
	// Registered last here, cwd restoration precedes env/root cleanup, but
	// follows the callers' later-registered cleanup (testing uses LIFO order).
	t.Chdir(root)
	return root
}
