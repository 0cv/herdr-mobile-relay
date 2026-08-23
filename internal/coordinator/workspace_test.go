package coordinator

import (
	"context"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func TestDirtyWorktreeRemovalOffersExplicitForceRetry(t *testing.T) {
	dispatcher := NewDispatcher(nil, NewState(testLogger()), nil, testLogger())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = dispatcher.Close(ctx)
	})

	result := dispatcher.failTopologyErr("remove-1", "worktree_remove", "", &herdr.OutcomeError{
		Started: true,
		Err: &herdr.CLIError{
			Code:    "dirty_worktree_requires_force",
			Message: "dirty checkout",
		},
	})
	if result.OK || result.Phase != "not_started" {
		t.Fatalf("result = %+v", result)
	}
	data, ok := result.Data.(map[string]any)
	if !ok || data["force_available"] != true {
		t.Fatalf("data = %#v", result.Data)
	}
}

func TestWorkspaceStatePreservesCwdAcrossMetadataEvents(t *testing.T) {
	state := NewState(testLogger())
	state.CommitWorkspaces([]herdr.Workspace{{ID: "w1", Label: "Project", Cwd: "/home/user/project"}})
	if !state.CommitWorkspaces([]herdr.Workspace{{ID: "w1", Label: "Renamed"}}) {
		t.Fatal("rename was not committed")
	}
	workspace, ok := state.Workspace("w1")
	if !ok || workspace.Label != "Renamed" || workspace.Cwd != "/home/user/project" {
		t.Fatalf("workspace = %+v, ok=%v", workspace, ok)
	}
}
