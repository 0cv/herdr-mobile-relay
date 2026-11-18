package coordinator

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
)

func TestDirtyWorktreeRemovalOffersExplicitForceRetry(t *testing.T) {
	dispatcher := newTestDispatcher(t, nil, NewState(testLogger()))

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

// A workspace mutation must release the hub's global ordered ingress as soon
// as its ordering position (topologyMu) is secured — not after the Herdr
// command completes, which can take the full command deadline.
func TestWorkspaceMutationDoesNotBlockIngressAdmission(t *testing.T) {
	client := newFakeBackend()
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	client.setHook("WorkspaceRename", func(ctx context.Context, _ fakeCall) error {
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	dispatcher := newTestDispatcher(t, client, NewState(testLogger()))
	t.Cleanup(unblock)
	dispatcher.state.CommitWorkspaces([]herdr.Workspace{{ID: "w1", Label: "Project"}})
	dispatcher.state.CommitInventory(
		[]*AgentState{{PaneID: "pane-1", Agent: "codex", Status: "working"}},
		dispatcher.state.RevisionCounter(),
	)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	admitted := make(chan struct{})
	renameDone := make(chan *CommandResult, 1)
	go func() {
		renameDone <- dispatcher.HandleTopologyAdmitted(
			ctx,
			func() { close(admitted) },
			func(ctx context.Context) *CommandResult {
				return dispatcher.HandleWorkspaceRename(ctx, "rename-1", "w1", "Renamed")
			},
		)
	}()

	select {
	case <-admitted:
	case <-time.After(time.Second):
		t.Fatal("workspace mutation was not admitted before its Herdr command completed")
	}
	select {
	case result := <-renameDone:
		t.Fatalf("rename completed before the stalled Herdr command was released: %+v", result)
	default:
	}

	prompt := dispatcher.Handle(ctx, protocol.Inbound{
		Type:      "submit_prompt",
		RequestID: "prompt",
		PaneID:    "pane-1",
		Text:      "continue",
	})
	if !prompt.OK {
		t.Fatalf("unrelated command was blocked behind the workspace mutation: %+v", prompt)
	}

	unblock()
	select {
	case result := <-renameDone:
		if !result.OK {
			t.Fatalf("rename result = %+v", result)
		}
	case <-ctx.Done():
		t.Fatal("rename did not complete after the Herdr command was released")
	}
}

// A topology mutation queued behind a running one must be admitted while the
// first still executes its Herdr command: its admission used to wait for
// topologyMu, which stalled the hub's global ordered ingress — prompts and
// approvals from every client — for the running command's full deadline.
func TestQueuedWorkspaceMutationDoesNotBlockIngressAdmission(t *testing.T) {
	client := newFakeBackend()
	client.workspaces = []herdr.Workspace{{ID: "w1", Label: "Project"}, {ID: "w2", Label: "Second"}}
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	client.setHook("WorkspaceRename", func(ctx context.Context, _ fakeCall) error {
		close(started)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	dispatcher := newTestDispatcher(t, client, NewState(testLogger()))
	t.Cleanup(unblock)
	dispatcher.state.CommitWorkspaces([]herdr.Workspace{
		{ID: "w1", Label: "Project"},
		{ID: "w2", Label: "Second"},
	})

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	renameDone := make(chan *CommandResult, 1)
	go func() {
		renameDone <- dispatcher.HandleWorkspaceRename(ctx, "rename-1", "w1", "Renamed")
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("rename never reached its Herdr command")
	}

	admitted := make(chan struct{})
	closeDone := make(chan *CommandResult, 1)
	go func() {
		closeDone <- dispatcher.HandleTopologyAdmitted(
			ctx,
			func() { close(admitted) },
			func(ctx context.Context) *CommandResult {
				return dispatcher.HandleWorkspaceClose(ctx, "close-1", "w2", false, nil)
			},
		)
	}()

	select {
	case <-admitted:
	case <-time.After(time.Second):
		t.Fatal("queued topology mutation was not admitted while the running one held topologyMu")
	}
	select {
	case result := <-closeDone:
		t.Fatalf("close completed before the running mutation released topologyMu: %+v", result)
	default:
	}

	unblock()
	for name, done := range map[string]chan *CommandResult{"rename": renameDone, "close": closeDone} {
		select {
		case result := <-done:
			if !result.OK {
				t.Fatalf("%s result = %+v", name, result)
			}
		case <-ctx.Done():
			t.Fatalf("%s did not complete after the Herdr command was released", name)
		}
	}
}
