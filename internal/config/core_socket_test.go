package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCoreSocketPath(t *testing.T) {
	root := t.TempDir()
	run := filepath.Join(root, "run")
	if err := os.Mkdir(run, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, override, socket, runtime, userRuntime, state, session, want string
	}{
		{"relay override", "/relay.sock", "/core.sock", "/xdg", run, "/state", "other", "/relay.sock"},
		{"core override", "", "/core.sock", "/xdg", run, "/state", "other", "/core.sock"},
		{"xdg runtime", "", "", "/xdg", run, "/state", "other", "/xdg/herdr-core/other.sock"},
		{"user runtime", "", "", "", run, "/state", "other", filepath.Join(run, "herdr-core/other.sock")},
		{"state", "", "", "", root + "/missing", "/state", "other", "/state/herdr-core/other/other.sock"},
		{"not a directory", "", "", "", file, "/state", "", "/state/herdr-core/default/default.sock"},
		{"home fallback", "", "", "", root + "/missing", "", "", filepath.Join(root, ".local/state/herdr-core/default/default.sock")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", root)
			t.Setenv("HC_SOCKET_PATH", tt.override)
			t.Setenv("HC_SOCKET", tt.socket)
			t.Setenv("XDG_RUNTIME_DIR", tt.runtime)
			t.Setenv("XDG_STATE_HOME", tt.state)
			t.Setenv("HC_SESSION", tt.session)
			if got := coreSocketPath(tt.userRuntime); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}
