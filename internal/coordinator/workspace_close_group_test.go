package coordinator

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

type closeSocketRequest struct {
	ID     string         `json:"id"`
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}

func startCloseSocket(t *testing.T, response func(closeSocketRequest) any) (string, <-chan closeSocketRequest) {
	t.Helper()
	socketPath := filepath.Join(t.TempDir(), "herdr.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	requests := make(chan closeSocketRequest, 4)
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			var request closeSocketRequest
			if json.NewDecoder(bufio.NewReader(conn)).Decode(&request) == nil {
				requests <- request
				if response != nil {
					_ = json.NewEncoder(conn).Encode(map[string]any{
						"id": request.ID, "result": response(request),
					})
				}
			}
			_ = conn.Close()
		}
	}()
	return socketPath, requests
}

func closeGroupWorkspaces() []herdr.Workspace {
	return []herdr.Workspace{
		{ID: "primary", Label: "Project", Worktree: &herdr.WorkspaceWorktree{RepoKey: "repo", IsLinkedWorktree: false}},
		{ID: "child", Label: "Fix", Worktree: &herdr.WorkspaceWorktree{RepoKey: "repo", IsLinkedWorktree: true}},
		{ID: "other", Label: "Other", Worktree: &herdr.WorkspaceWorktree{RepoKey: "other", IsLinkedWorktree: false}},
	}
}

func closeGroupResponse(request closeSocketRequest) any {
	if request.Method == "workspace.list" {
		return map[string]any{"type": "workspace_list", "workspaces": closeGroupWorkspaces()}
	}
	return map[string]any{"type": "ok"}
}

func closeGroupState() *State {
	state := NewState(testLogger())
	state.CommitWorkspaces(closeGroupWorkspaces())
	return state
}

func assertNoCloseRequest(t *testing.T, requests <-chan closeSocketRequest) {
	t.Helper()
	for {
		select {
		case request := <-requests:
			if request.Method == "workspace.close" {
				t.Fatalf("unexpected workspace.close request: %+v", request)
			}
		default:
			return
		}
	}
}

func waitForCloseRequest(t *testing.T, requests <-chan closeSocketRequest) closeSocketRequest {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case request := <-requests:
			if request.Method == "workspace.close" {
				return request
			}
		case <-deadline:
			t.Fatal("group close did not dispatch")
		}
	}
}

func TestWorkspaceClosePrimaryRequiresExplicitGroupConsent(t *testing.T) {
	socketPath, requests := startCloseSocket(t, closeGroupResponse)
	state := closeGroupState()
	dispatcher := NewDispatcher(herdr.NewClient("missing-herdr", socketPath), state, nil, testLogger())
	t.Cleanup(func() { _ = dispatcher.Close(context.Background()) })

	result := dispatcher.HandleWorkspaceClose(context.Background(), "close-1", "primary", false, nil)
	if result.OK || result.Phase != "not_started" || result.Data.(map[string]any)["code"] != "workspace_group_close_required" {
		t.Fatalf("result = %+v", result)
	}
	assertNoCloseRequest(t, requests)
}

func TestWorkspaceCloseGroupTargetsPrimaryOnce(t *testing.T) {
	socketPath, requests := startCloseSocket(t, closeGroupResponse)
	dispatcher := NewDispatcher(herdr.NewClient("missing-herdr", socketPath), closeGroupState(), nil, testLogger())
	t.Cleanup(func() { _ = dispatcher.Close(context.Background()) })

	result := dispatcher.HandleWorkspaceClose(context.Background(), "close-2", "primary", true, []string{"primary", "child"})
	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
	request := waitForCloseRequest(t, requests)
	if request.Params["workspace_id"] != "primary" || request.Params["close_group"] != true {
		t.Fatalf("request = %+v", request)
	}
	if _, ok := request.Params["expected_workspace_ids"]; ok {
		t.Fatalf("workspace.close sent speculative expected workspace ids: %+v", request.Params)
	}
	assertNoCloseRequest(t, requests)
	data := result.Data.(map[string]any)
	if data["close_group"] != true {
		t.Fatalf("result data = %#v", data)
	}
}

func TestWorkspaceCloseGroupRejectsLinkedChild(t *testing.T) {
	socketPath, requests := startCloseSocket(t, closeGroupResponse)
	dispatcher := NewDispatcher(herdr.NewClient("missing-herdr", socketPath), closeGroupState(), nil, testLogger())
	t.Cleanup(func() { _ = dispatcher.Close(context.Background()) })

	result := dispatcher.HandleWorkspaceClose(context.Background(), "close-3", "child", true, []string{"primary", "child"})
	if result.OK || result.Phase != "not_started" || result.Data.(map[string]any)["code"] != "workspace_group_primary_required" {
		t.Fatalf("result = %+v", result)
	}
	assertNoCloseRequest(t, requests)
}

func TestWorkspaceCloseGroupMembershipChangeRequiresReconfirmation(t *testing.T) {
	socketPath, requests := startCloseSocket(t, func(request closeSocketRequest) any {
		if request.Method == "workspace.list" {
			return map[string]any{
				"type": "workspace_list",
				"workspaces": []herdr.Workspace{
					{ID: "primary", Label: "Project", Worktree: &herdr.WorkspaceWorktree{RepoKey: "repo", IsLinkedWorktree: false}},
				},
			}
		}
		return map[string]any{"type": "ok"}
	})
	state := closeGroupState()
	state.CommitWorkspaces([]herdr.Workspace{
		{ID: "primary", Label: "Project", Worktree: &herdr.WorkspaceWorktree{RepoKey: "repo", IsLinkedWorktree: false}},
	})
	dispatcher := NewDispatcher(herdr.NewClient("missing-herdr", socketPath), state, nil, testLogger())
	t.Cleanup(func() { _ = dispatcher.Close(context.Background()) })

	result := dispatcher.HandleWorkspaceClose(context.Background(), "close-4", "primary", true, []string{"primary", "child"})
	if result.OK || result.Phase != "not_started" || result.Data.(map[string]any)["code"] != "workspace_group_changed" {
		t.Fatalf("result = %+v", result)
	}
	assertNoCloseRequest(t, requests)
}

func TestWorkspaceCloseGroupChecksAuthoritativeMembership(t *testing.T) {
	socketPath, requests := startCloseSocket(t, func(request closeSocketRequest) any {
		if request.Method == "workspace.list" {
			return map[string]any{
				"type": "workspace_list",
				"workspaces": append(closeGroupWorkspaces(), herdr.Workspace{
					ID:       "new-child",
					Label:    "New fix",
					Worktree: &herdr.WorkspaceWorktree{RepoKey: "repo", IsLinkedWorktree: true},
				}),
			}
		}
		return map[string]any{"type": "ok"}
	})
	dispatcher := NewDispatcher(herdr.NewClient("missing-herdr", socketPath), closeGroupState(), nil, testLogger())
	t.Cleanup(func() { _ = dispatcher.Close(context.Background()) })

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
	assertNoCloseRequest(t, requests)
}
