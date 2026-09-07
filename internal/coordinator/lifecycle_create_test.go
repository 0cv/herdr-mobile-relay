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

func TestLifecycleStartsAgentInNestedWorkspaceCreateRootPane(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	cwd := filepath.Join(home, "project")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatalf("create project directory: %v", err)
	}
	record := filepath.Join(dir, "invocations.log")
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"printf '%s\\n' \"$*\" >> \""+record+"\"\n"+
		"case \"$1 $2\" in\n"+
		"  'pane list') printf '%s\\n' '{\"result\":{\"panes\":[]}}' ;;\n"+
		"  'workspace list') printf '%s\\n' '{\"result\":{\"workspaces\":[]}}' ;;\n"+
		"  'workspace create') printf '%s\\n' '{\"result\":{\"type\":\"workspace_created\",\"workspace\":{\"workspace_id\":\"workspace-new\",\"label\":\"project\"},\"tab\":{\"tab_id\":\"tab-new\",\"workspace_id\":\"workspace-new\",\"label\":\"project\"},\"root_pane\":{\"pane_id\":\"pane-new\",\"tab_id\":\"tab-new\",\"workspace_id\":\"workspace-new\"}}}' ;;\n"+
		"  'tab rename') printf '%s\\n' '{\"result\":{\"tab_id\":\"tab-new\"}}' ;;\n"+
		"  'agent start') printf '%s\\n' '{\"result\":{\"type\":\"agent_started\",\"agent\":{\"pane_id\":\"pane-new\",\"agent\":\"codex\",\"name\":\"project-codex\"}}}' ;;\n"+
		"  *) exit 2 ;;\n"+
		"esac\n")

	resolver := profiles.NewResolver(filepath.Join(dir, "config"), nil)
	lifecycle := &Lifecycle{
		herdr:    herdr.NewClient(bin, filepath.Join(dir, "herdr.sock")),
		profiles: resolver,
		home:     home,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := lifecycle.Start(ctx, profiles.Profile{ID: "codex", Kind: "codex"}, StartRequest{
		ProfileID: "codex",
		Name:      "project-codex",
		Cwd:       cwd,
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if result.PaneID != "pane-new" {
		t.Fatalf("Start() pane_id = %q, want pane-new", result.PaneID)
	}

	invocations, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("read invocations: %v", err)
	}
	if !strings.Contains(string(invocations), "agent start project-codex --kind codex --pane pane-new") {
		t.Fatalf("agent was not started in created root pane:\n%s", invocations)
	}
}

func TestLifecycleStartPreflightsOwnershipPersistenceBeforeCreatingTarget(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	cwd := filepath.Join(home, "project")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "state")
	if err := os.WriteFile(statePath, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(dir, "invocations.log")
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"printf '%s\\n' \"$*\" >> \""+record+"\"\n"+
		"case \"$1 $2\" in\n"+
		"  'pane list') printf '%s\\n' '{\"result\":{\"panes\":[]}}' ;;\n"+
		"  'workspace list') printf '%s\\n' '{\"result\":{\"workspaces\":[]}}' ;;\n"+
		"  'workspace create') printf '%s\\n' '{\"result\":{\"type\":\"workspace_created\",\"workspace\":{\"workspace_id\":\"workspace-new\"},\"tab\":{\"tab_id\":\"tab-new\",\"workspace_id\":\"workspace-new\"},\"root_pane\":{\"pane_id\":\"pane-new\",\"tab_id\":\"tab-new\",\"workspace_id\":\"workspace-new\"}}}' ;;\n"+
		"  'tab rename'|'agent start') printf '%s\\n' '{\"result\":{}}' ;;\n"+
		"  *) exit 2 ;;\n"+
		"esac\n")
	lifecycle := &Lifecycle{
		herdr:    herdr.NewClient(bin, filepath.Join(dir, "herdr.sock")),
		profiles: profiles.NewResolver(filepath.Join(dir, "config"), nil, profiles.WithAssociationStore(statePath)),
		home:     home,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := lifecycle.Start(ctx, profiles.Profile{ID: "codex", Kind: "codex"}, StartRequest{ProfileID: "codex", Name: "project-codex", Cwd: cwd})
	if err == nil {
		t.Fatal("Start() reported success without durable ownership")
	}
	if result.PaneID != "" {
		t.Fatalf("Start() failure pane_id = %q, want no created target", result.PaneID)
	}
	invocations, readErr := os.ReadFile(record)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(invocations), "workspace create") || strings.Contains(string(invocations), "tab create") || strings.Contains(string(invocations), "agent start project-codex") {
		t.Fatalf("topology changed before ownership persistence succeeded:\n%s", invocations)
	}
	if got := lifecycle.profiles.ResolvePaneSession("pane-new", "unrelated-session", "copilot"); got != "" {
		t.Fatalf("failed preflight authorized a nonexistent pane as %q", got)
	}
}

func TestLifecycleStartReportsCreatedTargetWhenPersistenceFailsAfterPreflight(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	cwd := filepath.Join(home, "project")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(dir, "state")
	record := filepath.Join(dir, "invocations.log")
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"printf '%s\\n' \"$*\" >> \""+record+"\"\n"+
		"case \"$1 $2\" in\n"+
		"  'pane list') printf '%s\\n' '{\"result\":{\"panes\":[]}}' ;;\n"+
		"  'workspace list') printf '%s\\n' '{\"result\":{\"workspaces\":[]}}' ;;\n"+
		"  'workspace create') rm -f \""+filepath.Join(stateDir, "pane-profile-associations.json")+"\"; rmdir \""+stateDir+"\"; printf 'blocked\\n' > \""+stateDir+"\"; printf '%s\\n' '{\"result\":{\"workspace_id\":\"workspace-new\",\"tab_id\":\"tab-new\",\"pane_id\":\"pane-new\"}}' ;;\n"+
		"  'tab rename') printf '%s\\n' '{\"result\":{}}' ;;\n"+
		"  'agent start') printf '%s\\n' '{\"result\":{}}' ;;\n"+
		"  *) exit 2 ;;\n"+
		"esac\n")
	lifecycle := &Lifecycle{
		herdr:    herdr.NewClient(bin, filepath.Join(dir, "herdr.sock")),
		profiles: profiles.NewResolver(filepath.Join(dir, "config"), nil, profiles.WithAssociationStore(stateDir)),
		home:     home,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := lifecycle.Start(ctx, profiles.Profile{ID: "codex", Kind: "codex"}, StartRequest{ProfileID: "codex", Name: "project-codex", Cwd: cwd})
	if !errors.Is(err, herdr.ErrPartiallyApplied) || !errors.Is(err, herdr.ErrDispatchedUnknown) {
		t.Fatalf("post-create persistence error = %v, want truthful partial result", err)
	}
	if result.PaneID != "pane-new" || result.WorkspaceID != "workspace-new" {
		t.Fatalf("post-create persistence result = %+v", result)
	}
	invocations, readErr := os.ReadFile(record)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(invocations), "workspace create") || strings.Contains(string(invocations), "agent start project-codex") {
		t.Fatalf("post-create persistence boundary was not preserved:\n%s", invocations)
	}
}

func TestLifecyclePreservesDispatchBoundaryForMalformedSuccessfulAgentStart(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	cwd := filepath.Join(home, "project")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(dir, "state")
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"case \"$1 $2\" in\n"+
		"  'agent list') printf '%s\\n' '{\"result\":{\"agents\":[]}}' ;;\n"+
		"  'workspace list') printf '%s\\n' '{\"result\":{\"workspaces\":[]}}' ;;\n"+
		"  'workspace create') printf '%s\\n' '{\"result\":{\"workspace_id\":\"workspace-new\",\"tab_id\":\"tab-new\",\"pane_id\":\"pane-new\"}}' ;;\n"+
		"  'tab rename') printf '%s\\n' '{\"result\":{}}' ;;\n"+
		"  'agent start') printf '%s\\n' 'not-json' ;;\n"+
		"  *) exit 2 ;;\n"+
		"esac\n")
	lifecycle := &Lifecycle{
		herdr:    herdr.NewClient(bin, filepath.Join(dir, "herdr.sock")),
		profiles: profiles.NewResolver(filepath.Join(dir, "config"), nil, profiles.WithAssociationStore(stateDir)),
		home:     home,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := lifecycle.Start(ctx, profiles.Profile{ID: "codex", Kind: "codex"}, StartRequest{ProfileID: "codex", Name: "project-codex", Cwd: cwd})
	if !errors.Is(err, herdr.ErrDispatchedUnknown) || errors.Is(err, herdr.ErrNotStarted) {
		t.Fatalf("malformed successful start error = %v, want dispatched unknown", err)
	}
	if result.PaneID != "pane-new" {
		t.Fatalf("malformed successful start pane = %q", result.PaneID)
	}
}

func TestAgentStartAPIReportsMissingSuccessfulEnvelopeAsDispatchedUnknown(t *testing.T) {
	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(dir, "bin")
	configHome := filepath.Join(dir, "config")
	if err := os.MkdirAll(filepath.Join(configHome, "herdr"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "codex"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configHome, "herdr", "agent-profiles.ini"), []byte("[config]\nreplace_profiles = true\n[profiles]\ncodex = Codex\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	herdrBin := writeScript(t, dir, "herdr-api", "#!/bin/sh\n"+
		"case \"$1 $2\" in\n"+
		"  'agent list') printf '%s\\n' '{\"result\":{\"agents\":[]}}' ;;\n"+
		"  'workspace list') printf '%s\\n' '{\"result\":{\"workspaces\":[]}}' ;;\n"+
		"  'workspace create') printf '%s\\n' '{\"result\":{\"workspace_id\":\"workspace-new\",\"tab_id\":\"tab-new\",\"pane_id\":\"pane-new\"}}' ;;\n"+
		"  'tab rename') printf '%s\\n' '{\"result\":{}}' ;;\n"+
		"  'agent start') printf '%s\\n' '{\"ok\":true}' ;;\n"+
		"  *) exit 2 ;;\n"+
		"esac\n")
	dispatcher := NewDispatcher(herdr.NewClient(herdrBin, filepath.Join(dir, "herdr.sock")), NewState(testLogger()), nil, testLogger())
	dispatcher.SetProfiles(profiles.NewResolver(configHome, nil, profiles.WithAssociationStore(filepath.Join(dir, "state"))))
	t.Cleanup(func() { _ = dispatcher.Close(context.Background()) })

	result := dispatcher.Handle(t.Context(), map[string]any{
		"action": "agent_start", "request_id": "start", "profile_id": "codex", "name": "project-codex", "cwd": cwd,
	})
	if result.OK || result.Phase != "dispatched_unknown" || result.PaneID != "pane-new" {
		t.Fatalf("agent start API result = %+v", result)
	}
}

func TestLifecycleStartsAgentInExplicitWorkspace(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	cwd := filepath.Join(home, "project")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(dir, "invocations.log")
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"printf '%s\\n' \"$*\" >> \""+record+"\"\n"+
		"case \"$1 $2\" in\n"+
		"  'pane list') printf '%s\\n' '{\"result\":{\"panes\":[]}}' ;;\n"+
		"  'workspace list') printf '%s\\n' '{\"result\":{\"workspaces\":[{\"workspace_id\":\"workspace-existing\",\"label\":\"Project\"}]}}' ;;\n"+
		"  'tab create') printf '%s\\n' '{\"result\":{\"type\":\"tab_created\",\"tab\":{\"tab_id\":\"tab-new\",\"workspace_id\":\"workspace-existing\"},\"root_pane\":{\"pane_id\":\"pane-new\",\"tab_id\":\"tab-new\",\"workspace_id\":\"workspace-existing\"}}}' ;;\n"+
		"  'agent start') printf '%s\\n' '{\"result\":{\"type\":\"agent_started\",\"agent\":{\"pane_id\":\"pane-new\",\"agent\":\"codex\",\"name\":\"project-codex\"}}}' ;;\n"+
		"  *) exit 2 ;;\n"+
		"esac\n")

	lifecycle := &Lifecycle{
		herdr:    herdr.NewClient(bin, filepath.Join(dir, "herdr.sock")),
		profiles: profiles.NewResolver(filepath.Join(dir, "config"), nil),
		home:     home,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := lifecycle.Start(ctx, profiles.Profile{ID: "codex", Kind: "codex"}, StartRequest{
		ProfileID:   "codex",
		WorkspaceID: "workspace-existing",
		Name:        "project-codex",
		Cwd:         cwd,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.WorkspaceID != "workspace-existing" || result.PaneID != "pane-new" {
		t.Fatalf("result = %+v", result)
	}
	invocations, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(invocations), "tab create --workspace workspace-existing") {
		t.Fatalf("explicit workspace was not used:\n%s", invocations)
	}
	if strings.Contains(string(invocations), "workspace create") {
		t.Fatalf("unexpected workspace create:\n%s", invocations)
	}
}

func TestLifecycleReconcilesExistingPaneWithDurableOwnership(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	cwd := filepath.Join(home, "project")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	resolvedCwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		t.Fatal(err)
	}
	cwd = resolvedCwd
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "codex"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	herdrBin := writeScript(t, root, "herdr-existing", "#!/bin/sh\nif [ \"$1 $2\" = \"agent list\" ]; then printf '%s\\n' '{\"result\":{\"agents\":[{\"pane_id\":\"pane-existing\",\"workspace_id\":\"workspace-existing\",\"agent\":\"codex\",\"name\":\"existing\",\"cwd\":\""+cwd+"\",\"agent_session\":{\"value\":\"session-existing\"}}]}}'; else exit 1; fi\n")
	request := StartRequest{ProfileID: "codex", WorkspaceID: "workspace-existing", Name: "existing", Cwd: cwd}

	newSeededResolver := func(stateDir string) *profiles.Resolver {
		resolver := profiles.NewResolver(filepath.Join(root, "config"), nil, profiles.WithAssociationStore(stateDir))
		if err := resolver.Remember("pane-existing", "codex"); err != nil {
			t.Fatal(err)
		}
		if err := resolver.Reconcile([]profiles.Observation{{PaneID: "pane-existing", NativeSessionID: "session-existing"}}); err != nil {
			t.Fatal(err)
		}
		return resolver
	}

	t.Run("success", func(t *testing.T) {
		resolver := newSeededResolver(t.TempDir())
		lifecycle := &Lifecycle{herdr: herdr.NewClient(herdrBin, filepath.Join(root, "herdr.sock")), profiles: resolver, home: home}
		if got := resolver.ResolvePaneSession("pane-existing", "session-existing", "codex"); got != "codex" {
			t.Fatalf("seeded ownership = %q", got)
		}
		inventory, err := lifecycle.herdr.GetInventory(context.Background())
		if err != nil || len(inventory.Panes) != 1 {
			t.Fatalf("existing inventory = %+v, %v", inventory, err)
		}
		if pane := inventory.Panes[0]; pane.Name != request.Name || pane.Cwd != request.Cwd || pane.WorkspaceID != request.WorkspaceID || pane.Session != "session-existing" {
			t.Fatalf("existing pane = %+v", pane)
		}
		if got := lifecycle.reconcileExisting(context.Background(), "codex", request); got != "pane-existing" {
			t.Fatalf("reconciled pane = %q", got)
		}
		result, err := lifecycle.Start(context.Background(), profiles.Profile{ID: "codex"}, request)
		if err != nil || result.PaneID != "pane-existing" {
			t.Fatalf("existing start = %+v, %v", result, err)
		}
	})

	t.Run("persistence failure", func(t *testing.T) {
		stateDir := t.TempDir()
		resolver := newSeededResolver(stateDir)
		if err := os.RemoveAll(stateDir); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(stateDir, []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		lifecycle := &Lifecycle{herdr: herdr.NewClient(herdrBin, filepath.Join(root, "herdr.sock")), profiles: resolver, home: home}
		if got := lifecycle.reconcileExisting(context.Background(), "codex", request); got != "pane-existing" {
			t.Fatalf("reconciled pane = %q", got)
		}
		result, err := lifecycle.Start(context.Background(), profiles.Profile{ID: "codex"}, request)
		if err == nil || result.PaneID != "pane-existing" || !strings.Contains(err.Error(), "persist agent profile ownership") {
			t.Fatalf("existing persistence failure = %+v, %v", result, err)
		}
	})

	t.Run("ownership mismatch", func(t *testing.T) {
		resolver := profiles.NewResolver(filepath.Join(root, "config-mismatch"), nil)
		lifecycle := &Lifecycle{herdr: herdr.NewClient(herdrBin, filepath.Join(root, "herdr.sock")), profiles: resolver, home: home}
		if paneID := lifecycle.reconcileExisting(context.Background(), "personal", request); paneID != "" {
			t.Fatalf("ownership mismatch reconciled pane %q", paneID)
		}
	})
}

func TestLifecycleReportsRefusedStartWhenOwnershipCleanupCannotPersist(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	cwd := filepath.Join(home, "project")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "codex"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	stateDir := filepath.Join(root, "profile-state")
	herdrBin := writeScript(t, root, "herdr-refused", "#!/bin/sh\ncase \"$1 $2\" in\n  'agent list') printf '%s\\n' '{\"result\":{\"agents\":[]}}' ;;\n  'workspace list') printf '%s\\n' '{\"result\":{\"workspaces\":[]}}' ;;\n  'workspace create') printf '%s\\n' '{\"result\":{\"workspace\":{\"workspace_id\":\"workspace-new\"},\"tab\":{\"tab_id\":\"tab-new\",\"workspace_id\":\"workspace-new\"},\"root_pane\":{\"pane_id\":\"pane-new\",\"tab_id\":\"tab-new\",\"workspace_id\":\"workspace-new\"}}}' ;;\n  'tab rename') printf '%s\\n' '{\"result\":{}}' ;;\n  'agent start') if [ -d \""+stateDir+"\" ]; then mv \""+stateDir+"\" \""+stateDir+".saved\"; printf broken > \""+stateDir+"\"; fi; printf '%s\\n' '"+paneBusyEnvelope+"' >&2; exit 1 ;;\n  *) exit 2 ;;\nesac\n")
	resolver := profiles.NewResolver(filepath.Join(root, "config"), nil, profiles.WithAssociationStore(stateDir))
	lifecycle := &Lifecycle{herdr: herdr.NewClient(herdrBin, filepath.Join(root, "herdr.sock")), profiles: resolver, home: home}
	ctx, cancel := context.WithTimeout(context.Background(), agentStartResponseReserve+2*time.Second)
	defer cancel()
	result, err := lifecycle.Start(ctx, profiles.Profile{ID: "codex", Kind: "codex"}, StartRequest{ProfileID: "codex", Name: "new-agent", Cwd: cwd})
	if err == nil || result.PaneID != "pane-new" || !errors.Is(err, herdr.ErrPartiallyApplied) || !strings.Contains(err.Error(), "ownership intent cleanup") {
		t.Fatalf("refused cleanup failure = %+v, %v", result, err)
	}
}
