package coordinator

import (
	"context"
	"reflect"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func closeGroupWorkspaces() []herdr.Workspace {
	return []herdr.Workspace{
		{ID: "primary", Label: "Project", Worktree: &herdr.WorkspaceWorktree{RepoKey: "repo", IsLinkedWorktree: false}},
		{ID: "child", Label: "Fix", Worktree: &herdr.WorkspaceWorktree{RepoKey: "repo", IsLinkedWorktree: true}},
		{ID: "other", Label: "Other", Worktree: &herdr.WorkspaceWorktree{RepoKey: "other", IsLinkedWorktree: false}},
	}
}

func closeGroupBackend(workspaces []herdr.Workspace) *fakeBackend {
	client := newFakeBackend()
	client.workspaces = workspaces
	return client
}

func assertNoCloseCall(t *testing.T, client *fakeBackend) {
	t.Helper()
	if calls := client.callsTo("WorkspaceClose"); len(calls) != 0 {
		t.Fatalf("unexpected WorkspaceClose calls: %+v", calls)
	}
}

func closeGroupState() *State {
	state := NewState(testLogger())
	state.CommitWorkspaces(closeGroupWorkspaces())
	return state
}

func TestWorkspaceClosePrimaryRequiresExplicitGroupConsent(t *testing.T) {
	client := closeGroupBackend(closeGroupWorkspaces())
	dispatcher := newTestDispatcher(t, client, closeGroupState())

	result := dispatcher.HandleWorkspaceClose(context.Background(), "close-1", "primary", false, nil)
	if result.OK || result.Phase != "not_started" || result.Data.(map[string]any)["code"] != "workspace_group_close_required" {
		t.Fatalf("result = %+v", result)
	}
	assertNoCloseCall(t, client)
}

func TestWorkspaceCloseGroupTargetsPrimaryOnce(t *testing.T) {
	client := closeGroupBackend(closeGroupWorkspaces())
	dispatcher := newTestDispatcher(t, client, closeGroupState())

	result := dispatcher.HandleWorkspaceClose(context.Background(), "close-2", "primary", true, []string{"primary", "child"})
	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	want := []fakeCall{{Method: "WorkspaceClose", Args: []any{"primary", true}}}
	if got := client.callsTo("WorkspaceClose"); !reflect.DeepEqual(got, want) {
		t.Fatalf("close calls = %+v, want %+v", got, want)
	}
	data := result.Data.(map[string]any)
	if data["close_group"] != true {
		t.Fatalf("result data = %#v", data)
	}
}

func TestWorkspaceCloseGroupRejectsLinkedChild(t *testing.T) {
	client := closeGroupBackend(closeGroupWorkspaces())
	dispatcher := newTestDispatcher(t, client, closeGroupState())

	result := dispatcher.HandleWorkspaceClose(context.Background(), "close-3", "child", true, []string{"primary", "child"})
	if result.OK || result.Phase != "not_started" || result.Data.(map[string]any)["code"] != "workspace_group_primary_required" {
		t.Fatalf("result = %+v", result)
	}
	assertNoCloseCall(t, client)
}

func TestWorkspaceCloseGroupMembershipChangeRequiresReconfirmation(t *testing.T) {
	client := closeGroupBackend([]herdr.Workspace{
		{ID: "primary", Label: "Project", Worktree: &herdr.WorkspaceWorktree{RepoKey: "repo", IsLinkedWorktree: false}},
	})
	state := closeGroupState()
	state.CommitWorkspaces([]herdr.Workspace{
		{ID: "primary", Label: "Project", Worktree: &herdr.WorkspaceWorktree{RepoKey: "repo", IsLinkedWorktree: false}},
	})
	dispatcher := newTestDispatcher(t, client, state)

	result := dispatcher.HandleWorkspaceClose(context.Background(), "close-4", "primary", true, []string{"primary", "child"})
	if result.OK || result.Phase != "not_started" || result.Data.(map[string]any)["code"] != "workspace_group_changed" {
		t.Fatalf("result = %+v", result)
	}
	assertNoCloseCall(t, client)
}

func TestWorkspaceCloseGroupChecksAuthoritativeMembership(t *testing.T) {
	client := closeGroupBackend(append(closeGroupWorkspaces(), herdr.Workspace{
		ID:       "new-child",
		Label:    "New fix",
		Worktree: &herdr.WorkspaceWorktree{RepoKey: "repo", IsLinkedWorktree: true},
	}))
	dispatcher := newTestDispatcher(t, client, closeGroupState())

	result := dispatcher.HandleWorkspaceClose(
		context.Background(),
		"close-live-membership",
		"primary",
		true,
		[]string{"primary", "child"},
	)
	if result.OK || result.Phase != "not_started" ||
		result.Data.(map[string]any)["code"] != "workspace_group_changed" {
		t.Fatalf("result = %+v", result)
	}
	assertNoCloseCall(t, client)
}
