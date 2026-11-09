package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
)

func TestHermesRegistry(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "runtime")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "active_sessions.json")
	for _, tc := range []struct {
		name, data, want string
		bad              bool
	}{
		{"missing", "", "", false},
		{"no match", `{"entries":[{"surface":"cli","pid":43,"process_start_time":100,"session_id":"s"}]}`, "", false},
		{"single", `{"entries":[{"surface":"cli","pid":42,"process_start_time":100,"session_id":"s"}]}`, "s", false},
		{"wrong start", `{"entries":[{"surface":"cli","pid":42,"process_start_time":99,"session_id":"s"}]}`, "", false},
		{"missing start", `{"entries":[{"surface":"cli","pid":42,"session_id":"s"}]}`, "", false},
		{"other surface", `{"entries":[{"surface":"desktop","pid":42,"process_start_time":100,"session_id":"s"}]}`, "", false},
		{"multiple", `{"entries":[{"surface":"cli","pid":42,"process_start_time":100,"session_id":"s"},{"surface":"cli","pid":42,"process_start_time":100,"session_id":"s"}]}`, "", false},
		{"malformed", "{", "", true},
		{"oversized", strings.Repeat(" ", maxSessionRegistry+1), "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.data != "" {
				if err := os.WriteFile(path, []byte(tc.data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			entries, err := readRegistryEntries([]string{path}, "hermes")
			if (err != nil) != tc.bad {
				t.Fatal(err)
			}
			if got := matchRegistrySession(entries, 42, 100); got != tc.want {
				t.Fatalf("got %q", got)
			}
		})
	}
}

func TestEnrichHermesSessions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("HERMES_HOME", home)
	t.Setenv(agentroots.HermesListEnv, "")
	start, err := registryProcessStart(t.Context(), os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "runtime")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "active_sessions.json")
	data, _ := json.Marshal(map[string]any{"entries": []registryEntry{{Session: "session", Surface: "cli", PID: os.Getpid(), Start: start}}})
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	processes := &fixtureProcesses{pid: os.Getpid()}
	s := &Server{cfg: &config.Config{Backend: "hc"}, herdrC: &discoveryBackend{fixtureProcesses: processes}}
	check := func(agent, existing, want string) {
		t.Helper()
		a := &coordinator.AgentState{Agent: agent, PaneID: "pane", Session: existing}
		s.enrichRegistrySessions(t.Context(), []*coordinator.AgentState{a})
		if a.Session != want {
			t.Fatalf("got %q want %q", a.Session, want)
		}
	}
	check("hermes", "", "session")
	check("hermes-agent", "", "session")
	check("hermes", "label", "label")
	processes.pid++
	check("hermes", "", "")
	processes.pid--
	s.cfg.Backend = "herdr"
	calls := processes.calls
	check("hermes", "", "")
	if calls != processes.calls {
		t.Fatal("TUI discovery")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(data) {
		t.Fatal("registry changed")
	}
}
