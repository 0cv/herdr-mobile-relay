package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"testing"
)

func TestWorkspaceListKeepsWorktreeMetadata(t *testing.T) {
	socketPath, done := startUnaryTestSocket(t, "workspace.list", map[string]any{
		"type": "workspace_list",
		"workspaces": []any{map[string]any{
			"workspace_id":  "w1",
			"number":        1,
			"label":         "Project",
			"pane_count":    1,
			"tab_count":     1,
			"active_tab_id": "t1",
			"agent_status":  "idle",
			"worktree": map[string]any{
				"repo_key":           "repo",
				"repo_name":          "project",
				"repo_root":          "/home/user/project",
				"checkout_path":      "/home/user/project",
				"is_linked_worktree": false,
			},
		}},
	})
	client := NewClient(filepath.Join(t.TempDir(), "missing-herdr"), socketPath)

	workspaces, err := client.WorkspaceList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(workspaces) != 1 || workspaces[0].ID != "w1" || workspaces[0].Worktree == nil {
		t.Fatalf("workspaces = %#v", workspaces)
	}
	if workspaces[0].Worktree.RepoName != "project" || workspaces[0].Worktree.IsLinkedWorktree {
		t.Fatalf("worktree metadata = %#v", workspaces[0].Worktree)
	}
	if socketErr := <-done; socketErr != nil {
		t.Fatal(socketErr)
	}
}

func TestWorktreeListParsesSourceAndOpenWorkspace(t *testing.T) {
	bin := writeResultScript(t, `{"result":{"type":"worktree_list","source":{"repo_key":"repo","repo_name":"project","repo_root":"/home/user/project","source_checkout_path":"/home/user/project","source_workspace_id":"w1"},"worktrees":[{"path":"/home/user/worktrees/fix","branch":"fix/one","is_bare":false,"is_detached":false,"is_prunable":false,"is_linked_worktree":true,"label":"fix/one","open_workspace_id":"w2"}]}}`)
	client := NewClient(bin, filepath.Join(t.TempDir(), "herdr.sock"))

	result, err := client.WorktreeList(context.Background(), "w1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Source.RepoName != "project" || len(result.Worktrees) != 1 {
		t.Fatalf("result = %#v", result)
	}
	if result.Worktrees[0].Branch == nil || *result.Worktrees[0].Branch != "fix/one" ||
		result.Worktrees[0].OpenWorkspaceID == nil || *result.Worktrees[0].OpenWorkspaceID != "w2" {
		t.Fatalf("worktree = %#v", result.Worktrees[0])
	}
}

func TestWorktreeCreateParsesWorkspaceAndRootPane(t *testing.T) {
	bin := writeResultScript(t, `{"result":{"type":"worktree_created","workspace":{"workspace_id":"w2","number":2,"label":"fix/one","worktree":{"repo_key":"repo","repo_name":"project","repo_root":"/home/user/project","checkout_path":"/home/user/worktrees/fix","is_linked_worktree":true}},"tab":{"tab_id":"t2","workspace_id":"w2","label":"fix/one","cwd":"/home/user/worktrees/fix"},"root_pane":{"pane_id":"p2","tab_id":"t2","workspace_id":"w2","cwd":"/home/user/worktrees/fix"},"worktree":{"path":"/home/user/worktrees/fix","branch":"fix/one","is_bare":false,"is_detached":false,"is_prunable":false,"is_linked_worktree":true,"label":"fix/one","open_workspace_id":"w2"}}}`)
	client := NewClient(bin, filepath.Join(t.TempDir(), "herdr.sock"))

	result, err := client.WorktreeCreate(context.Background(), "w1", "fix/one", "main", "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Workspace.ID != "w2" || result.RootPane.ID != "p2" || result.Worktree.Path != "/home/user/worktrees/fix" {
		t.Fatalf("result = %#v", result)
	}
}

func TestSupportsWorkspaceMoveBlockUsesLiveServerProbe(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "herdr.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		defer listener.Close()
		for range 3 {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				done <- acceptErr
				return
			}
			var request struct {
				ID     string `json:"id"`
				Method string `json:"method"`
			}
			decodeErr := json.NewDecoder(bufio.NewReader(conn)).Decode(&request)
			if decodeErr != nil {
				_ = conn.Close()
				done <- decodeErr
				return
			}
			var response map[string]any
			switch request.Method {
			case "ping":
				response = map[string]any{
					"id": request.ID,
					"result": map[string]any{
						"type": "pong", "version": "0.9.0", "protocol": 1,
					},
				}
			case "workspace.move_block":
				response = map[string]any{
					"id": request.ID,
					"error": map[string]any{
						"code": "workspace_move_block_failed", "message": "empty selection",
					},
				}
			default:
				response = map[string]any{
					"id":    request.ID,
					"error": map[string]any{"code": "unknown_method", "message": "unknown"},
				}
			}
			if encodeErr := json.NewEncoder(conn).Encode(response); encodeErr != nil {
				_ = conn.Close()
				done <- encodeErr
				return
			}
			_ = conn.Close()
		}
		done <- nil
	}()
	client := NewClient(filepath.Join(t.TempDir(), "missing-herdr"), socketPath)
	client.RefreshCapabilities(context.Background())
	if !client.SupportsWorkspaceMoveBlock() {
		t.Fatal("live workspace.move_block refusal did not prove support")
	}
	if socketErr := <-done; socketErr != nil {
		t.Fatal(socketErr)
	}
}

func TestWorkspaceCloseEOFIsDispatchedUnknownWithoutRetry(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "herdr.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	requests := make(chan string, 2)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		var request struct {
			Method string `json:"method"`
		}
		if json.NewDecoder(bufio.NewReader(conn)).Decode(&request) == nil {
			requests <- request.Method
		}
	}()

	client := NewClient("missing-herdr", socketPath)
	err = client.WorkspaceClose(context.Background(), "w1", true)
	if err == nil || !errors.Is(err, ErrDispatchedUnknown) || errors.Is(err, ErrNotStarted) {
		t.Fatalf("WorkspaceClose() error = %v, want one dispatched-unknown mutation", err)
	}
	if method := <-requests; method != "workspace.close" {
		t.Fatalf("request method = %q, want workspace.close", method)
	}
	select {
	case method := <-requests:
		t.Fatalf("WorkspaceClose() retried after EOF with %q", method)
	default:
	}
}

// A malformed or missing result envelope after the subprocess reported
// success means the mutation may have applied: the failure must classify as
// dispatched-unknown, never as a plain retryable failure.
func TestWorktreeMutationDecodeFailureIsDispatchedUnknown(t *testing.T) {
	tests := []struct {
		name string
		call func(client *Client) error
	}{
		{"create", func(client *Client) error {
			_, err := client.WorktreeCreate(context.Background(), "w1", "fix/one", "", "")
			return err
		}},
		{"open", func(client *Client) error {
			_, err := client.WorktreeOpen(context.Background(), "w1", "", "fix/one", "")
			return err
		}},
		{"remove", func(client *Client) error {
			_, err := client.WorktreeRemove(context.Background(), "w2", false)
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bin := writeResultScript(t, `{"result":null}`)
			client := NewClient(bin, filepath.Join(t.TempDir(), "herdr.sock"))
			err := test.call(client)
			if err == nil {
				t.Fatal("malformed envelope did not fail")
			}
			if !errors.Is(err, ErrDispatchedUnknown) {
				t.Fatalf("err = %v, want ErrDispatchedUnknown", err)
			}
			if errors.Is(err, ErrNotStarted) {
				t.Fatalf("err = %v classifies a possibly-applied mutation as retry-safe", err)
			}
		})
	}
}

// A subprocess that never started keeps its retry-safe classification even
// though the decode wrapper runs on the same path.
func TestWorktreeMutationNotStartedStaysRetrySafe(t *testing.T) {
	client := NewClient(filepath.Join(t.TempDir(), "missing-binary"), filepath.Join(t.TempDir(), "herdr.sock"))
	_, err := client.WorktreeRemove(context.Background(), "w2", false)
	if !errors.Is(err, ErrNotStarted) {
		t.Fatalf("err = %v, want ErrNotStarted", err)
	}
	if errors.Is(err, ErrDispatchedUnknown) {
		t.Fatalf("err = %v classifies an unstarted subprocess as dispatched-unknown", err)
	}
}
