package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
)

func TestGrokRegistry(t *testing.T) {
	row := `{"session_id":"session","pid":42,"pid_start":100000000,"cwd":"/fixture","opened_at":"2026-11-02T12:00:00Z"}`
	for _, tc := range []struct {
		name, data, want string
		bad              bool
	}{
		{"missing", "", "", false},
		{"none", "[]", "", false},
		{"single", "[" + row + "]", "session", false},
		{"several", "[" + row + "," + row + "]", "", false},
		{"wrong pid", strings.ReplaceAll("["+row+"]", `"pid":42`, `"pid":43`), "", false},
		{"wrong start", strings.ReplaceAll("["+row+"]", "100000000", "99000000"), "", false},
		{"within tolerance", strings.ReplaceAll("["+row+"]", "100000000", "100000500"), "session", false},
		{"outside tolerance", strings.ReplaceAll("["+row+"]", "100000000", "100002000"), "", false},
		{"malformed", "[", "", true},
		{"oversized", strings.Repeat(" ", maxSessionRegistry+1), "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "active_sessions.json")
			if tc.data != "" {
				if err := os.WriteFile(path, []byte(tc.data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			entries, err := readRegistryEntries([]string{path}, "grok")
			if (err != nil) != tc.bad {
				t.Fatalf("error=%v", err)
			}
			if got := matchRegistrySession(entries, 42, 100); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestEnrichGrokSessions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	start, err := registryProcessStart(t.Context(), os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".grok")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "active_sessions.json")
	data := fmt.Sprintf(`[{"session_id":"session","pid":%d,"pid_start":%d,"cwd":"/fixture","opened_at":"2026-11-02T12:00:00Z"}]`, os.Getpid(), int64(start*1e6))
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	processes := &fixtureProcesses{pid: os.Getpid()}
	s := &Server{cfg: &config.Config{Backend: "hc"}, herdrC: &discoveryBackend{fixtureProcesses: processes}}
	check := func(agent, existing, want string) {
		t.Helper()
		a := &coordinator.AgentState{Agent: agent, PaneID: "t13", Session: existing}
		s.enrichRegistrySessions(t.Context(), []*coordinator.AgentState{a})
		if a.Session != want {
			t.Fatalf("got %q want %q", a.Session, want)
		}
	}
	check("grok", "", "session")
	check("grok-build", "", "session")
	check("grok-build", "existing", "existing")
	processes.pid++
	check("grok-build", "", "")
	processes.pid--
	s.cfg.Backend = "herdr"
	calls := processes.calls
	check("grok-build", "", "")
	if processes.calls != calls {
		t.Fatal("TUI registry discovery")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != data {
		t.Fatal("registry changed")
	}
}
