package coordinator

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/profiles"
)

func TestLifecycleStartsAgentInNestedWorkspaceCreateRootPane(t *testing.T) {
	env := newLaunchEnv(t, "codex")
	client := newLaunchFake()
	lifecycle := &Lifecycle{herdr: client, profiles: profiles.NewResolver(env.config, nil), home: env.home}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := lifecycle.Start(ctx, profiles.Profile{ID: "codex", Kind: "codex"}, StartRequest{
		ProfileID: "codex",
		Name:      "project-codex",
		Cwd:       env.cwd,
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if result.PaneID != "pane-new" {
		t.Fatalf("Start() pane_id = %q, want pane-new", result.PaneID)
	}

	starts := client.callsTo("StartAgent")
	if len(starts) != 1 || !reflect.DeepEqual(starts[0].Args[:3], []any{"project-codex", "codex", "pane-new"}) {
		t.Fatalf("agent was not started in created root pane: %+v", client.recorded())
	}
}

func TestLifecycleStartsAgentInExplicitWorkspace(t *testing.T) {
	env := newLaunchEnv(t, "codex")
	client := newLaunchFake()
	client.target.WorkspaceID = "workspace-existing"
	client.workspaces = []herdr.Workspace{{ID: "workspace-existing", Label: "Project"}}
	lifecycle := &Lifecycle{herdr: client, profiles: profiles.NewResolver(env.config, nil), home: env.home}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := lifecycle.Start(ctx, profiles.Profile{ID: "codex", Kind: "codex"}, StartRequest{
		ProfileID:   "codex",
		WorkspaceID: "workspace-existing",
		Name:        "project-codex",
		Cwd:         env.cwd,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.WorkspaceID != "workspace-existing" || result.PaneID != "pane-new" {
		t.Fatalf("result = %+v", result)
	}
	tabs := client.callsTo("TabCreate")
	if len(tabs) != 1 || tabs[0].Args[0] != "workspace-existing" {
		t.Fatalf("explicit workspace was not used: %+v", client.recorded())
	}
	if client.count("WorkspaceCreate") != 0 {
		t.Fatalf("unexpected workspace create: %+v", client.recorded())
	}
}
