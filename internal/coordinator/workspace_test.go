package coordinator

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/profiles"
)

func TestTopologyMutationsReachTheLockedExecutionBoundary(t *testing.T) {
	dir := t.TempDir()
	client := herdr.NewClient(writeScript(t, dir, "herdr-fail", "#!/bin/sh\nexit 1\n"), filepath.Join(dir, "missing.sock"))
	dispatcher := NewDispatcher(client, NewState(testLogger()), nil, testLogger())
	dispatcher.SetProfiles(profiles.NewResolver(filepath.Join(dir, "config"), nil))
	dispatcher.state.CommitWorkspaces([]herdr.Workspace{{ID: "w1", Label: "One"}, {ID: "w2", Label: "Two"}})
	t.Cleanup(func() { _ = dispatcher.Close(context.Background()) })
	index := 0
	for name, run := range map[string]func() *CommandResult{
		"create":  func() *CommandResult { return dispatcher.HandleWorkspaceCreate(t.Context(), "create", dir, "Created") },
		"reorder": func() *CommandResult { return dispatcher.HandleWorkspaceReorder(t.Context(), "reorder", "w1", &index) },
		"reorder block": func() *CommandResult {
			return dispatcher.HandleWorkspaceReorderBlock(t.Context(), "block", []string{"w1", "w2"}, "")
		},
		"worktree create": func() *CommandResult {
			return dispatcher.HandleWorktreeCreate(t.Context(), "worktree-create", "w1", "branch", "", "", "Branch")
		},
		"worktree open": func() *CommandResult {
			return dispatcher.HandleWorktreeOpen(t.Context(), "worktree-open", "w1", "", "branch", "Branch")
		},
	} {
		t.Run(name, func(t *testing.T) {
			if result := run(); result.OK {
				t.Fatalf("mutation unexpectedly succeeded: %+v", result)
			}
		})
	}
}

func TestWorkspaceCreateAndWorktreeMutationsCommitFleetChanges(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "project")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	bin := writeScript(t, root, "herdr-success", "#!/bin/sh\ncase \"$1 $2\" in\n  'workspace create') printf '%s\\n' '{\"result\":{\"workspace\":{\"workspace_id\":\"workspace-new\"},\"tab\":{\"tab_id\":\"tab-new\",\"workspace_id\":\"workspace-new\"},\"root_pane\":{\"pane_id\":\"pane-new\",\"tab_id\":\"tab-new\",\"workspace_id\":\"workspace-new\"}}}' ;;\n  'worktree create'|'worktree open') printf '%s\\n' '{\"result\":{\"workspace\":{\"workspace_id\":\"worktree-new\",\"label\":\"Branch\"},\"tab\":{\"tab_id\":\"tab-worktree\",\"workspace_id\":\"worktree-new\"},\"root_pane\":{\"pane_id\":\"pane-worktree\",\"workspace_id\":\"worktree-new\"},\"worktree\":{\"path\":\"/worktree\",\"label\":\"Branch\",\"is_linked_worktree\":true}}}' ;;\n  *) exit 1 ;;\nesac\n")
	state := NewState(testLogger())
	state.CommitWorkspaces([]herdr.Workspace{{ID: "workspace-source", Label: "Source"}})
	dispatcher := NewDispatcher(herdr.NewClient(bin, filepath.Join(root, "herdr.sock")), state, nil, testLogger())
	dispatcher.SetProfiles(profiles.NewResolver(filepath.Join(root, "config"), nil))
	dispatcher.lifecycle.home = root
	t.Cleanup(func() { _ = dispatcher.Close(context.Background()) })

	results := []*CommandResult{
		dispatcher.HandleWorkspaceCreate(t.Context(), "create", cwd, "Created"),
		dispatcher.HandleWorktreeCreate(t.Context(), "worktree-create", "workspace-source", "branch", "", "", "Branch"),
		dispatcher.HandleWorktreeOpen(t.Context(), "worktree-open", "workspace-source", "", "branch", "Branch"),
	}
	for index, result := range results {
		if !result.OK {
			t.Fatalf("mutation %d = %+v", index, result)
		}
	}
	if got := dispatcher.fleetEpoch.Load(); got != 3 {
		t.Fatalf("fleet epoch = %d, want 3", got)
	}
}

func TestTopologyEffectRejectsFenceConflictAndEmptyFailedStart(t *testing.T) {
	dispatcher := NewDispatcher(nil, NewState(testLogger()), nil, testLogger())
	t.Cleanup(func() { _ = dispatcher.Close(context.Background()) })
	blocked := &CommandResult{RequestID: "blocked", Phase: "not_started", Error: "blocked"}
	fencedCtx := WithExecutionFence(context.Background(), func() *CommandResult { return blocked })
	result := dispatcher.topologyEffect(fencedCtx, "request", CommandStop, "pane", EffectFunc(func(context.Context, WorkerToken) EffectResult {
		t.Fatal("fenced topology effect ran")
		return EffectResult{}
	})).Run(context.Background(), WorkerToken{})
	if result.Result != blocked {
		t.Fatalf("fenced result = %+v", result.Result)
	}

	conflicted := dispatcher.topologyEffect(context.Background(), "request", CommandStop, "pane", EffectFunc(func(context.Context, WorkerToken) EffectResult {
		t.Fatal("conflicted topology effect ran")
		return EffectResult{}
	}))
	dispatcher.fleetEpoch.Add(1)
	result = conflicted.Run(context.Background(), WorkerToken{})
	if result.Result == nil || result.Result.Phase != "not_started" {
		t.Fatalf("fleet conflict = %+v", result.Result)
	}

	before := dispatcher.fleetEpoch.Load()
	result = dispatcher.topologyEffect(context.Background(), "request", CommandStart, "", EffectFunc(func(context.Context, WorkerToken) EffectResult {
		return EffectResult{Result: &CommandResult{RequestID: "request", Action: "agent_start", Phase: "not_started"}}
	})).Run(context.Background(), WorkerToken{})
	if result.Result == nil || result.Result.OK || dispatcher.fleetEpoch.Load() != before {
		t.Fatalf("empty failed start changed fleet: result=%+v epoch=%d", result.Result, dispatcher.fleetEpoch.Load())
	}

	before = dispatcher.fleetEpoch.Load()
	result = dispatcher.topologyEffect(context.Background(), "request", CommandStop, "pane", EffectFunc(func(context.Context, WorkerToken) EffectResult {
		return EffectResult{Result: &CommandResult{RequestID: "request", Action: "agent_stop", Phase: "dispatched_unknown"}}
	})).Run(context.Background(), WorkerToken{})
	if result.Result == nil || result.Result.Phase != "dispatched_unknown" || dispatcher.fleetEpoch.Load() != before+1 {
		t.Fatalf("unknown topology outcome did not invalidate queued mutations: result=%+v epoch=%d", result.Result, dispatcher.fleetEpoch.Load())
	}

	before = dispatcher.fleetEpoch.Load()
	result = dispatcher.topologyEffect(context.Background(), "request", CommandStop, "pane", EffectFunc(func(context.Context, WorkerToken) EffectResult {
		return EffectResult{BumpGeneration: true}
	})).Run(context.Background(), WorkerToken{})
	if result.Result != nil || dispatcher.fleetEpoch.Load() != before+1 {
		t.Fatalf("result-free topology effect did not honor explicit generation bump: result=%+v epoch=%d", result.Result, dispatcher.fleetEpoch.Load())
	}
}

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
			"rename-1", "workspace_rename",
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
			"close-1", "workspace_close",
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
	if result := <-renameDone; !result.OK {
		t.Fatalf("rename result = %+v", result)
	}
	if result := <-closeDone; !result.OK {
		t.Fatalf("close result = %+v", result)
	}
}

func TestAgentStartAndWorkspaceCloseShareOneTopologyExecutor(t *testing.T) {
	dir := t.TempDir()
	startEntered := filepath.Join(dir, "start-entered")
	closeEntered := filepath.Join(dir, "close-entered")
	release := filepath.Join(dir, "release-start")
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"case \"$1 $2\" in\n"+
		"  'agent start') touch \""+startEntered+"\"; while [ ! -f \""+release+"\" ]; do sleep 0.01; done; printf '%s\\n' '{\"result\":{\"pane_id\":\"pane-new\"}}' ;;\n"+
		"  'workspace close') touch \""+closeEntered+"\"; printf '%s\\n' '{\"result\":{}}' ;;\n"+
		"  *) printf '%s\\n' '{\"result\":{}}' ;;\n"+
		"esac\n")
	state := NewState(testLogger())
	state.CommitWorkspaces([]herdr.Workspace{{ID: "w1", Label: "Project"}})
	dispatcher := NewDispatcher(herdr.NewClient(bin, filepath.Join(dir, "sock")), state, nil, testLogger())
	t.Cleanup(func() {
		_ = os.WriteFile(release, nil, 0o600)
		_ = dispatcher.Close(context.Background())
	})
	startDone := make(chan *CommandResult, 1)
	go func() {
		startDone <- dispatcher.Handle(t.Context(), map[string]any{
			"action": "agent_start", "request_id": "start", "profile_id": "codex", "name": "new", "cwd": "/tmp/project",
		})
	}()
	waitForOwnershipTestPath(t, startEntered)

	admitted := make(chan struct{})
	closeDone := make(chan *CommandResult, 1)
	go func() {
		closeDone <- dispatcher.HandleTopologyAdmitted(t.Context(), "close", "workspace_close", func() { close(admitted) }, func(ctx context.Context) *CommandResult {
			return dispatcher.HandleWorkspaceClose(ctx, "close", "w1")
		})
	}()
	<-admitted
	time.Sleep(30 * time.Millisecond)
	if _, err := os.Stat(closeEntered); err == nil {
		t.Fatal("workspace close overlapped the in-flight agent start")
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if result := <-startDone; !result.OK {
		t.Fatalf("start result = %+v", result)
	}
	if result := <-closeDone; result.OK || result.Phase != "not_started" {
		t.Fatalf("stale queued close result = %+v", result)
	}
	if _, err := os.Stat(closeEntered); err == nil {
		t.Fatal("stale workspace close executed after agent start")
	}
}

func TestAgentClearAndWorktreeRemoveShareOneTopologyExecutor(t *testing.T) {
	dir := t.TempDir()
	clearEntered := filepath.Join(dir, "clear-entered")
	removeEntered := filepath.Join(dir, "remove-entered")
	release := filepath.Join(dir, "release-clear")
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"case \"$1 $2\" in\n"+
		"  'pane close') touch \""+clearEntered+"\"; while [ ! -f \""+release+"\" ]; do sleep 0.01; done; printf '%s\\n' '{\"result\":{}}' ;;\n"+
		"  'worktree remove') touch \""+removeEntered+"\"; printf '%s\\n' '{\"result\":{}}' ;;\n"+
		"  *) printf '%s\\n' '{\"result\":{}}' ;;\n"+
		"esac\n")
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Agent: "codex", Status: "idle", WorkspaceID: "w1"}}, state.RevisionCounter())
	state.CommitWorkspaces([]herdr.Workspace{{ID: "w1", Label: "Linked", Worktree: &herdr.WorkspaceWorktree{IsLinkedWorktree: true}}})
	dispatcher := NewDispatcher(herdr.NewClient(bin, filepath.Join(dir, "sock")), state, nil, testLogger())
	t.Cleanup(func() {
		_ = os.WriteFile(release, nil, 0o600)
		_ = dispatcher.Close(context.Background())
	})
	clearDone := make(chan *CommandResult, 1)
	go func() {
		clearDone <- dispatcher.Handle(t.Context(), map[string]any{"action": "agent_clear", "request_id": "clear", "pane_id": "pane-1"})
	}()
	waitForOwnershipTestPath(t, clearEntered)

	admitted := make(chan struct{})
	removeDone := make(chan *CommandResult, 1)
	go func() {
		removeDone <- dispatcher.HandleTopologyAdmitted(t.Context(), "remove", "worktree_remove", func() { close(admitted) }, func(ctx context.Context) *CommandResult {
			return dispatcher.HandleWorktreeRemove(ctx, "remove", "w1", false)
		})
	}()
	<-admitted
	time.Sleep(30 * time.Millisecond)
	if _, err := os.Stat(removeEntered); err == nil {
		t.Fatal("worktree removal overlapped the in-flight agent clear")
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if result := <-clearDone; !result.OK {
		t.Fatalf("clear result = %+v", result)
	}
	if result := <-removeDone; result.OK || result.Phase != "not_started" {
		t.Fatalf("stale queued remove result = %+v", result)
	}
	if _, err := os.Stat(removeEntered); err == nil {
		t.Fatal("stale worktree remove executed after agent clear")
	}
}
