package coordinator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", WorkspaceID: "workspace-1", Agent: "copilot", Status: "idle", Cwd: dir}}, state.RevisionCounter())
	state.CommitWorkspaces([]herdr.Workspace{{
		ID: "workspace-1", Label: "Unknown owner", Worktree: &herdr.WorkspaceWorktree{IsLinkedWorktree: true},
	}})
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
	for action, run := range map[string]func() *CommandResult{
		"workspace_close": func() *CommandResult {
			return dispatcher.HandleWorkspaceClose(context.Background(), "workspace-close", "workspace-1")
		},
		"worktree_remove": func() *CommandResult {
			return dispatcher.HandleWorktreeRemove(context.Background(), "worktree-remove", "workspace-1", true)
		},
	} {
		result := run()
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
	dispatcher.state.CommitInventory(nil, dispatcher.state.RevisionCounter())
	if result := dispatcher.denyUnknownWorkspaceProfileOwnership("request", "workspace_close", "empty-workspace"); result != nil {
		t.Fatalf("empty workspace denial = %+v", result)
	}
	if result := dispatcher.denyUnknownProfileOwnership("request", "agent_stop", "missing"); result != nil {
		t.Fatalf("absent target denial = %+v", result)
	}
	dispatcher.state.CommitInventory([]*AgentState{{PaneID: "known", Agent: "codex", Status: "idle"}}, dispatcher.state.RevisionCounter())
	if result := dispatcher.denyUnknownProfileOwnership("request", "agent_stop", "known"); result != nil {
		t.Fatalf("known target denial = %+v", result)
	}
}

func TestProfileOwnershipGuardFailsClosedAfterInventoryFailure(t *testing.T) {
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Agent: "copilot", SessionID: "session-1", Status: "idle"}}, 0)
	resolver := profiles.NewResolver(filepath.Join(t.TempDir(), "config"), nil)
	if err := resolver.Remember("pane-1", "personal"); err != nil {
		t.Fatal(err)
	}
	dispatcher := NewDispatcher(nil, state, nil, testLogger())
	dispatcher.profiles = resolver
	if denied := dispatcher.denyUnknownProfileOwnership("before", "agent_stop", "pane-1"); denied != nil {
		t.Fatalf("ready ownership denied = %+v", denied)
	}
	state.MarkInventoryFailure(errors.New("inventory unavailable"))
	if denied := dispatcher.denyUnknownProfileOwnership("after", "agent_stop", "pane-1"); denied == nil || denied.Error != unknownProfileOwnershipError {
		t.Fatalf("stale ownership denial = %+v", denied)
	}
}

func TestAgentStopDoesNotReportSuccessUntilOwnershipRemovalIsDurable(t *testing.T) {
	dir := t.TempDir()
	configHome := filepath.Join(dir, "config")
	binDir := filepath.Join(dir, "bin")
	stateDir := filepath.Join(dir, "profile-state")
	if err := os.MkdirAll(filepath.Join(configHome, "herdr"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "personal"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configHome, "herdr", "agent-profiles.ini"), []byte("[config]\nreplace_profiles = true\n[profiles]\npersonal = Personal\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	resolver := profiles.NewResolver(configHome, nil, profiles.WithAssociationStore(stateDir))
	if err := resolver.Remember("pane-1", "personal"); err != nil {
		t.Fatal(err)
	}
	if err := resolver.Reconcile([]profiles.Observation{{PaneID: "pane-1", NativeSessionID: "session-1"}}); err != nil {
		t.Fatal(err)
	}
	invocations := filepath.Join(dir, "herdr-invocations")
	herdrBin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"printf '%s\\n' \"$*\" >> \""+invocations+"\"\n"+
		"if [ \"$1 $2\" = \"pane close\" ]; then mv \""+stateDir+"\" \""+stateDir+".saved\"; printf broken > \""+stateDir+"\"; fi\n"+
		"printf '%s\\n' '{\"result\":{}}'\n")
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Agent: "copilot", Status: "idle", SessionID: "session-1"}}, state.RevisionCounter())
	dispatcher := NewDispatcher(herdr.NewClient(herdrBin, filepath.Join(dir, "sock")), state, nil, testLogger())
	dispatcher.SetProfiles(resolver)
	t.Cleanup(func() { _ = dispatcher.Close(context.Background()) })

	result := dispatcher.handleStop(t.Context(), time.Now(), "stop", "pane-1")
	if result.OK || result.Phase != "dispatched_unknown" {
		t.Fatalf("stop result = %+v, want durable-cleanup failure", result)
	}
	data, err := os.ReadFile(invocations)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "pane close pane-1") {
		t.Fatalf("pane was not stopped before persistence failure:\n%s", data)
	}
}

func TestAgentStopCommitsSuccessfulOwnershipRemoval(t *testing.T) {
	root := t.TempDir()
	herdrBin := writeScript(t, root, "herdr-stop", "#!/bin/sh\nprintf '%s\\n' '{\"result\":{}}'\n")
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Agent: "codex", Status: "idle", SessionID: "session-1"}}, state.RevisionCounter())
	resolver := profiles.NewResolver(filepath.Join(root, "config"), nil)
	if err := resolver.Remember("pane-1", "codex"); err != nil {
		t.Fatal(err)
	}
	dispatcher := NewDispatcher(herdr.NewClient(herdrBin, filepath.Join(root, "herdr.sock")), state, nil, testLogger())
	dispatcher.profiles = resolver
	t.Cleanup(func() { _ = dispatcher.Close(context.Background()) })
	if result := dispatcher.handleStop(t.Context(), time.Now(), "stop", "pane-1"); !result.OK {
		t.Fatalf("successful stop = %+v", result)
	}
}

func TestAgentClearReportsOwnershipRemovalFailureAfterReplacement(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	cwd := filepath.Join(home, "project")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(root, "bin")
	configHome := filepath.Join(root, "config")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "codex"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	stateDir := filepath.Join(root, "profile-state")
	resolver := profiles.NewResolver(configHome, nil, profiles.WithAssociationStore(stateDir))
	if err := resolver.Remember("pane-old", "codex"); err != nil {
		t.Fatal(err)
	}
	if err := resolver.Reconcile([]profiles.Observation{{PaneID: "pane-old", NativeSessionID: "session-old"}}); err != nil {
		t.Fatal(err)
	}
	herdrBin := writeScript(t, root, "herdr-clear", "#!/bin/sh\ncase \"$1 $2\" in\n  'agent list') printf '%s\\n' '{\"result\":{\"agents\":[]}}' ;;\n  'workspace list') printf '%s\\n' '{\"result\":{\"workspaces\":[]}}' ;;\n  'workspace create') printf '%s\\n' '{\"result\":{\"workspace\":{\"workspace_id\":\"workspace-new\"},\"tab\":{\"tab_id\":\"tab-new\",\"workspace_id\":\"workspace-new\"},\"root_pane\":{\"pane_id\":\"pane-new\",\"tab_id\":\"tab-new\",\"workspace_id\":\"workspace-new\"}}}' ;;\n  'tab rename') printf '%s\\n' '{\"result\":{}}' ;;\n  'agent start') printf '%s\\n' '{\"result\":{\"agent\":{\"pane_id\":\"pane-new\"}}}' ;;\n  'pane close') if [ -d \""+stateDir+"\" ]; then mv \""+stateDir+"\" \""+stateDir+".saved\"; printf broken > \""+stateDir+"\"; fi; printf '%s\\n' '{\"result\":{}}' ;;\n  *) exit 2 ;;\nesac\n")
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{PaneID: "pane-old", Agent: "codex", Status: "idle", SessionID: "session-old", Cwd: cwd}}, state.RevisionCounter())
	dispatcher := NewDispatcher(herdr.NewClient(herdrBin, filepath.Join(root, "herdr.sock")), state, nil, testLogger())
	dispatcher.SetProfiles(resolver)
	dispatcher.lifecycle.home = home
	t.Cleanup(func() { _ = dispatcher.Close(context.Background()) })
	result := dispatcher.handleClear(t.Context(), time.Now(), "clear", "pane-old")
	if result.OK || result.Phase != "dispatched_unknown" || result.PaneID != "pane-old" {
		t.Fatalf("clear ownership failure = %+v", result)
	}
}

func TestFallbackClearAllowsKnownProfileOwnershipInsideWorker(t *testing.T) {
	dir := t.TempDir()
	configHome := filepath.Join(dir, "config")
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(filepath.Join(configHome, "herdr"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "personal"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configHome, "herdr", "agent-profiles.ini"), []byte("[config]\nreplace_profiles = true\n[profiles]\npersonal = Personal\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Agent: "copilot", Status: "idle"}}, state.RevisionCounter())
	dispatcher := NewDispatcher(herdr.NewClient(writeScript(t, dir, "herdr", "#!/bin/sh\nexit 0\n"), filepath.Join(dir, "sock")), state, nil, testLogger())
	resolver := profiles.NewResolver(configHome, nil)
	resolver.Remember("pane-1", "personal")
	dispatcher.profiles = resolver
	t.Cleanup(func() { _ = dispatcher.Close(context.Background()) })
	if result := dispatcher.handleClear(context.Background(), time.Now(), "clear", "pane-1"); !result.OK {
		t.Fatalf("known fallback clear = %+v", result)
	}
}

func TestFallbackClearRevalidatesUnknownProfileOwnershipInsideWorker(t *testing.T) {
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Agent: "copilot", Status: "idle"}}, state.RevisionCounter())
	dispatcher := NewDispatcher(nil, state, nil, testLogger())
	dispatcher.profiles = profiles.NewResolver(filepath.Join(t.TempDir(), "config"), nil)
	t.Cleanup(func() { _ = dispatcher.Close(context.Background()) })

	result := dispatcher.handleClear(context.Background(), time.Now(), "clear", "pane-1")
	if result.OK || result.Error != unknownProfileOwnershipError {
		t.Fatalf("fallback clear result = %+v, want stable ownership denial", result)
	}
}

func TestClearRevalidatesOldPaneOwnershipAfterStartingReplacement(t *testing.T) {
	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	started := filepath.Join(dir, "replacement-started")
	release := filepath.Join(dir, "release-replacement")
	invocations := filepath.Join(dir, "herdr-invocations")
	herdrBin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"printf '%s\\n' \"$*\" >> \""+invocations+"\"\n"+
		"case \"$1 $2\" in\n"+
		"  'pane list') printf '%s\\n' '{\"result\":{\"panes\":[]}}' ;;\n"+
		"  'workspace list') printf '%s\\n' '{\"result\":{\"workspaces\":[]}}' ;;\n"+
		"  'workspace create') touch \""+started+"\"; while [ ! -f \""+release+"\" ]; do sleep 0.01; done; printf '%s\\n' '{\"result\":{\"type\":\"workspace_created\",\"workspace\":{\"workspace_id\":\"workspace-new\",\"label\":\"project\"},\"tab\":{\"tab_id\":\"tab-new\",\"workspace_id\":\"workspace-new\",\"label\":\"project\"},\"root_pane\":{\"pane_id\":\"pane-new\",\"tab_id\":\"tab-new\",\"workspace_id\":\"workspace-new\"}}}' ;;\n"+
		"  'tab rename'|'pane run'|'agent rename') printf '%s\\n' '{\"result\":{}}' ;;\n"+
		"  'agent get') printf '%s\\n' '{\"result\":{\"pane_id\":\"pane-new\",\"running\":true,\"status\":\"idle\"}}' ;;\n"+
		"  *) printf '%s\\n' '{\"result\":{}}' ;;\n"+
		"esac\n")
	binDir := filepath.Join(dir, "bin")
	configHome := filepath.Join(dir, "config")
	if err := os.MkdirAll(filepath.Join(configHome, "herdr"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "personal"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configHome, "herdr", "agent-profiles.ini"), []byte("[config]\nreplace_profiles = true\n[profiles]\npersonal = Personal\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Agent: "copilot", Status: "idle", Cwd: cwd}}, state.RevisionCounter())
	dispatcher := NewDispatcher(herdr.NewClient(herdrBin, filepath.Join(dir, "sock")), state, nil, testLogger())
	resolver := profiles.NewResolver(configHome, nil)
	resolver.Remember("pane-1", "personal")
	dispatcher.SetProfiles(resolver)
	t.Cleanup(func() {
		_ = os.WriteFile(release, nil, 0o600)
		_ = dispatcher.Close(context.Background())
	})
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if result := dispatcher.handleClear(t.Context(), time.Now(), "clear-success", "pane-1"); !result.OK {
		t.Fatalf("exact-owned clear = %+v", result)
	}
	for _, path := range []string{release, started, invocations} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Agent: "copilot", Status: "idle", Cwd: cwd}}, state.RevisionCounter())
	resolver.Remember("pane-1", "personal")

	done := make(chan *CommandResult, 1)
	go func() { done <- dispatcher.handleClear(t.Context(), time.Now(), "clear", "pane-1") }()
	waitForOwnershipTestPath(t, started)
	resolver.Forget("pane-1")
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	result := <-done
	if result.OK || result.Error != unknownProfileOwnershipError {
		t.Fatalf("post-start clear result = %+v, want stable ownership denial", result)
	}
	data, err := os.ReadFile(invocations)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "pane run pane-new") || strings.Contains(string(data), "pane close pane-1") {
		t.Fatalf("replacement/old-pane boundary invocations:\n%s", data)
	}
}

func TestWorktreeRemoveRevalidatesAndAllowsExactProfileOwnership(t *testing.T) {
	dir := t.TempDir()
	configHome := filepath.Join(dir, "config")
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(filepath.Join(configHome, "herdr"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "personal"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configHome, "herdr", "agent-profiles.ini"), []byte("[config]\nreplace_profiles = true\n[profiles]\npersonal = Personal\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	herdrBin := writeScript(t, dir, "herdr", "#!/bin/sh\nprintf '%s\\n' '{\"result\":{\"workspace_id\":\"workspace-1\",\"path\":\"/worktree\",\"forced\":true}}'\n")
	state := NewState(testLogger())
	state.CommitWorkspaces([]herdr.Workspace{{ID: "workspace-1", Label: "Owned", Worktree: &herdr.WorkspaceWorktree{IsLinkedWorktree: true}}})
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", WorkspaceID: "workspace-1", Agent: "copilot", Status: "idle"}}, state.RevisionCounter())
	dispatcher := NewDispatcher(herdr.NewClient(herdrBin, filepath.Join(dir, "sock")), state, nil, testLogger())
	resolver := profiles.NewResolver(configHome, nil)
	resolver.Remember("pane-1", "personal")
	dispatcher.SetProfiles(resolver)
	t.Cleanup(func() { _ = dispatcher.Close(context.Background()) })
	if result := dispatcher.HandleWorktreeRemove(context.Background(), "remove", "workspace-1", true); !result.OK {
		t.Fatalf("exact-owned worktree removal = %+v", result)
	}
}

func TestQueuedPaneDestructionRevalidatesProfileOwnership(t *testing.T) {
	for _, action := range []string{"agent_stop", "agent_clear", "agent_restart"} {
		t.Run(action, func(t *testing.T) {
			dir := t.TempDir()
			cwd, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			started := filepath.Join(dir, "prompt-started")
			release := filepath.Join(dir, "release-prompt")
			invocations := filepath.Join(dir, "herdr-invocations")
			herdrBin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
				"printf '%s\\n' \"$*\" >> \""+invocations+"\"\n"+
				"if [ \"$1 $2\" = \"agent prompt\" ]; then\n"+
				"  touch \""+started+"\"\n"+
				"  while [ ! -f \""+release+"\" ]; do sleep 0.01; done\n"+
				"fi\n"+
				"printf '{\"ok\":true}\\n'\n")
			binDir := filepath.Join(dir, "bin")
			configHome := filepath.Join(dir, "config")
			if err := os.MkdirAll(filepath.Join(configHome, "herdr"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(binDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(binDir, "personal"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(configHome, "herdr", "agent-profiles.ini"), []byte("[config]\nreplace_profiles = true\n[profiles]\npersonal = Personal\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			state := NewState(testLogger())
			state.CommitInventory([]*AgentState{{PaneID: "pane-1", Agent: "copilot", Status: "idle", Cwd: cwd}}, state.RevisionCounter())
			dispatcher := NewDispatcher(herdr.NewClient(herdrBin, filepath.Join(dir, "sock")), state, nil, testLogger())
			resolver := profiles.NewResolver(configHome, nil)
			resolver.Remember("pane-1", "personal")
			dispatcher.SetProfiles(resolver)
			t.Cleanup(func() {
				_ = os.WriteFile(release, nil, 0o600)
				_ = dispatcher.Close(context.Background())
			})

			promptDone := make(chan *CommandResult, 1)
			go func() {
				promptDone <- dispatcher.Handle(t.Context(), map[string]any{
					"action": "submit_prompt", "request_id": "prompt", "pane_id": "pane-1", "text": "hold",
				})
			}()
			waitForOwnershipTestPath(t, started)

			admitted := make(chan struct{})
			destructiveDone := make(chan *CommandResult, 1)
			go func() {
				destructiveDone <- dispatcher.HandleAdmitted(t.Context(), map[string]any{
					"action": action, "request_id": action, "pane_id": "pane-1",
				}, func() { close(admitted) })
			}()
			select {
			case <-admitted:
			case <-time.After(time.Second):
				t.Fatal("destructive command was not admitted while prompt held the pane worker")
			}

			resolver.Forget("pane-1")
			if err := os.WriteFile(release, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if result := <-promptDone; !result.OK {
				t.Fatalf("blocking prompt result = %+v", result)
			}
			result := <-destructiveDone
			if result.OK || result.Error != unknownProfileOwnershipError {
				t.Fatalf("queued %s result = %+v, want stable ownership denial", action, result)
			}
			data, err := os.ReadFile(invocations)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "pane close pane-1") {
				t.Fatalf("queued %s destroyed a pane after ownership became unknown:\n%s", action, data)
			}
		})
	}
}

func TestQueuedWorkspaceDestructionRevalidatesProfileOwnership(t *testing.T) {
	for _, action := range []string{"workspace_close", "worktree_remove"} {
		t.Run(action, func(t *testing.T) {
			dir := t.TempDir()
			started := filepath.Join(dir, "rename-started")
			release := filepath.Join(dir, "release-rename")
			invocations := filepath.Join(dir, "herdr-invocations")
			herdrBin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
				"printf '%s\\n' \"$*\" >> \""+invocations+"\"\n"+
				"if [ \"$1 $2\" = \"workspace rename\" ]; then\n"+
				"  touch \""+started+"\"\n"+
				"  while [ ! -f \""+release+"\" ]; do sleep 0.01; done\n"+
				"fi\n"+
				"printf '{\"ok\":true}\\n'\n")
			state := NewState(testLogger())
			state.CommitWorkspaces([]herdr.Workspace{
				{ID: "workspace-1", Label: "Blocker"},
				{ID: "workspace-2", Label: "Protected", Worktree: &herdr.WorkspaceWorktree{IsLinkedWorktree: true}},
			})
			state.CommitInventory([]*AgentState{{PaneID: "pane-2", WorkspaceID: "workspace-2", Agent: "copilot", Status: "idle"}}, state.RevisionCounter())
			dispatcher := NewDispatcher(herdr.NewClient(herdrBin, filepath.Join(dir, "sock")), state, nil, testLogger())
			resolver := profiles.NewResolver(filepath.Join(dir, "config"), nil)
			resolver.Remember("pane-2", "personal")
			dispatcher.SetProfiles(resolver)
			t.Cleanup(func() {
				_ = os.WriteFile(release, nil, 0o600)
				_ = dispatcher.Close(context.Background())
			})

			renameDone := make(chan *CommandResult, 1)
			go func() {
				renameDone <- dispatcher.HandleWorkspaceRename(t.Context(), "rename", "workspace-1", "Held")
			}()
			waitForOwnershipTestPath(t, started)

			admitted := make(chan struct{})
			destructiveDone := make(chan *CommandResult, 1)
			go func() {
				destructiveDone <- dispatcher.HandleTopologyAdmitted(t.Context(), "destructive", action, func() { close(admitted) }, func(ctx context.Context) *CommandResult {
					if action == "workspace_close" {
						return dispatcher.HandleWorkspaceClose(ctx, action, "workspace-2")
					}
					return dispatcher.HandleWorktreeRemove(ctx, action, "workspace-2", true)
				})
			}()
			select {
			case <-admitted:
			case <-time.After(time.Second):
				t.Fatal("destructive workspace command was not admitted while rename held the topology worker")
			}

			resolver.Forget("pane-2")
			if err := os.WriteFile(release, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if result := <-renameDone; !result.OK {
				t.Fatalf("blocking rename result = %+v", result)
			}
			result := <-destructiveDone
			if result.OK || result.Error != unknownProfileOwnershipError {
				t.Fatalf("queued %s result = %+v, want stable ownership denial", action, result)
			}
			data, err := os.ReadFile(invocations)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "workspace close workspace-2") || strings.Contains(string(data), "worktree remove --workspace workspace-2") {
				t.Fatalf("queued %s destroyed a workspace after ownership became unknown:\n%s", action, data)
			}
		})
	}
}

func waitForOwnershipTestPath(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
