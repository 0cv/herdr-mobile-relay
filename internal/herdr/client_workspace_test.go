package herdr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWorkspaceListKeepsWorktreeMetadata(t *testing.T) {
	bin := writeResultScript(t, `{"result":{"type":"workspace_list","workspaces":[{"workspace_id":"w1","number":1,"label":"Project","pane_count":1,"tab_count":1,"active_tab_id":"t1","agent_status":"idle","worktree":{"repo_key":"repo","repo_name":"project","repo_root":"/home/user/project","checkout_path":"/home/user/project","is_linked_worktree":false}}]}}`)
	client := NewClient(bin, filepath.Join(t.TempDir(), "herdr.sock"))

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

func TestSupportsWorkspaceMoveBlockReadsBundledSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "herdr")
	schema := `{"schemas":{"request":{"oneOf":[{"properties":{"method":{"const":"workspace.move"}}},{"properties":{"method":{"const":"workspace.move_block"}}}]}}}`
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s\\n' '"+schema+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	client := NewClient(path, filepath.Join(t.TempDir(), "herdr.sock"))
	if !client.SupportsWorkspaceMoveBlock() {
		t.Fatal("workspace.move_block was not detected")
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
