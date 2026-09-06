package coordinator

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/profiles"
)

func TestUnknownCustomProfileOwnershipDeniesPaneDestruction(t *testing.T) {
	dir := t.TempDir()
	invocations := filepath.Join(dir, "herdr-invocations")
	herdrBin := writeScript(t, dir, "herdr", "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \""+invocations+"\"\nprintf '{\"ok\":true}\\n'\n")
	binDir := filepath.Join(dir, "bin")
	configHome := filepath.Join(dir, "config")
	if err := os.MkdirAll(filepath.Join(configHome, "herdr"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"personal", "emu"} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(configHome, "herdr", "agent-profiles.ini"), []byte("[config]\nreplace_profiles = true\n[profiles]\npersonal = Personal\nemu = EMU\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Agent: "copilot", Status: "idle", Cwd: dir}}, state.RevisionCounter())
	dispatcher := NewDispatcher(herdr.NewClient(herdrBin, filepath.Join(dir, "sock")), state, nil, testLogger())
	dispatcher.SetProfiles(profiles.NewResolver(configHome, nil))

	for _, action := range []string{"agent_stop", "agent_clear", "agent_restart"} {
		result := dispatcher.Handle(context.Background(), map[string]any{
			"action": action, "request_id": action, "pane_id": "pane-1",
		})
		if result.OK || result.Error != unknownProfileOwnershipError {
			t.Errorf("%s result = %+v, want stable ownership denial", action, result)
		}
	}
	if data, _ := os.ReadFile(invocations); len(data) != 0 {
		t.Fatalf("unknown ownership invoked Herdr:\n%s", data)
	}
}

func TestProfileOwnershipGuardAllowsKnownOrAbsentTargets(t *testing.T) {
	dispatcher := NewDispatcher(nil, NewState(testLogger()), nil, testLogger())
	if result := dispatcher.denyUnknownProfileOwnership("request", "agent_stop", "pane"); result != nil {
		t.Fatalf("nil resolver denial = %+v", result)
	}
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "codex"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	dispatcher.SetProfiles(profiles.NewResolver(filepath.Join(dir, "config"), nil))
	if result := dispatcher.denyUnknownProfileOwnership("request", "agent_stop", "missing"); result != nil {
		t.Fatalf("absent target denial = %+v", result)
	}
	dispatcher.state.CommitInventory([]*AgentState{{PaneID: "known", Agent: "codex", Status: "idle"}}, dispatcher.state.RevisionCounter())
	if result := dispatcher.denyUnknownProfileOwnership("request", "agent_stop", "known"); result != nil {
		t.Fatalf("known target denial = %+v", result)
	}
}
