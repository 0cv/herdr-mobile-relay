package coordinator

import (
	"context"
	"os"
	"path/filepath"
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

// A workspace mutation must release the hub's global ordered ingress as soon
// as its ordering position (topologyMu) is secured — not after the Herdr
// command completes, which can take the full command deadline.
func TestWorkspaceMutationDoesNotBlockIngressAdmission(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "rename-started")
	release := filepath.Join(dir, "release-rename")
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"if [ \"$1 $2\" = \"workspace rename\" ]; then\n"+
		"  touch \""+started+"\"\n"+
		"  while [ ! -f \""+release+"\" ]; do sleep 0.01; done\n"+
		"fi\n"+
		"printf '{\"ok\":true}\\n'\n")

	dispatcher := NewDispatcher(
		herdr.NewClient(bin, filepath.Join(dir, "sock")),
		NewState(testLogger()),
		nil,
		testLogger(),
	)
	t.Cleanup(func() {
		_ = os.WriteFile(release, nil, 0o600)
		_ = dispatcher.Close(context.Background())
	})
	dispatcher.state.CommitWorkspaces([]herdr.Workspace{{ID: "w1", Label: "Project"}})
	dispatcher.state.CommitInventory(
		[]*AgentState{{PaneID: "pane-1", Agent: "codex", Status: "working"}},
		dispatcher.state.RevisionCounter(),
	)

	admitted := make(chan struct{})
	renameDone := make(chan *CommandResult, 1)
	go func() {
		renameDone <- dispatcher.HandleTopologyAdmitted(
			t.Context(),
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

	promptCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	prompt := dispatcher.Handle(promptCtx, map[string]any{
		"action":     "submit_prompt",
		"request_id": "prompt",
		"pane_id":    "pane-1",
		"text":       "continue",
	})
	if !prompt.OK {
		t.Fatalf("unrelated command was blocked behind the workspace mutation: %+v", prompt)
	}

	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-renameDone:
		if !result.OK {
			t.Fatalf("rename result = %+v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("rename did not complete after the Herdr command was released")
	}
}

// A topology mutation queued behind a running one must be admitted while the
// first still executes its Herdr command: its admission used to wait for
// topologyMu, which stalled the hub's global ordered ingress — prompts and
// approvals from every client — for the running command's full deadline.
func TestQueuedWorkspaceMutationDoesNotBlockIngressAdmission(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "rename-started")
	release := filepath.Join(dir, "release-rename")
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"if [ \"$1 $2\" = \"workspace rename\" ]; then\n"+
		"  touch \""+started+"\"\n"+
		"  while [ ! -f \""+release+"\" ]; do sleep 0.01; done\n"+
		"fi\n"+
		"printf '{\"ok\":true}\\n'\n")

	dispatcher := NewDispatcher(
		herdr.NewClient(bin, filepath.Join(dir, "sock")),
		NewState(testLogger()),
		nil,
		testLogger(),
	)
	t.Cleanup(func() {
		_ = os.WriteFile(release, nil, 0o600)
		_ = dispatcher.Close(context.Background())
	})
	dispatcher.state.CommitWorkspaces([]herdr.Workspace{
		{ID: "w1", Label: "Project"},
		{ID: "w2", Label: "Second"},
	})

	renameDone := make(chan *CommandResult, 1)
	go func() {
		renameDone <- dispatcher.HandleWorkspaceRename(t.Context(), "rename-1", "w1", "Renamed")
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("rename never reached its Herdr command")
		}
		time.Sleep(5 * time.Millisecond)
	}

	admitted := make(chan struct{})
	closeDone := make(chan *CommandResult, 1)
	go func() {
		closeDone <- dispatcher.HandleTopologyAdmitted(
			t.Context(),
			func() { close(admitted) },
			func(ctx context.Context) *CommandResult {
				return dispatcher.HandleWorkspaceClose(ctx, "close-1", "w2")
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

	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, done := range map[string]chan *CommandResult{"rename": renameDone, "close": closeDone} {
		select {
		case result := <-done:
			if !result.OK {
				t.Fatalf("%s result = %+v", name, result)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s did not complete after the Herdr command was released", name)
		}
	}
}
