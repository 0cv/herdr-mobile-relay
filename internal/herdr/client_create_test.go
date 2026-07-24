package herdr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeResultScript(t *testing.T, result string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "herdr")
	body := "#!/bin/sh\nprintf '%s\\n' '" + result + "'\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write Herdr script: %v", err)
	}
	return path
}

func TestWorkspaceCreateParsesHerdr075NestedResult(t *testing.T) {
	bin := writeResultScript(t, `{"result":{"type":"workspace_created","workspace":{"workspace_id":"workspace-1","label":"Project"},"tab":{"tab_id":"tab-1","workspace_id":"workspace-1","label":"Project","cwd":"/home/user/project"},"root_pane":{"pane_id":"pane-1","tab_id":"tab-1","workspace_id":"workspace-1","cwd":"/home/user/project"}}}`)
	client := NewClient(bin, filepath.Join(t.TempDir(), "herdr.sock"))

	result, err := client.WorkspaceCreate(context.Background(), "/home/user/project", "Project")
	if err != nil {
		t.Fatalf("WorkspaceCreate() error = %v", err)
	}
	if result.PaneID != "pane-1" || result.TabID != "tab-1" || result.WorkspaceID != "workspace-1" {
		t.Fatalf("WorkspaceCreate() = %#v, want nested root-pane identifiers", result)
	}
}

func TestTabCreateParsesHerdr075NestedResult(t *testing.T) {
	bin := writeResultScript(t, `{"result":{"type":"tab_created","tab":{"tab_id":"tab-2","workspace_id":"workspace-1","label":"agent","cwd":"/home/user/project"},"root_pane":{"pane_id":"pane-2","tab_id":"tab-2","workspace_id":"workspace-1","cwd":"/home/user/project"}}}`)
	client := NewClient(bin, filepath.Join(t.TempDir(), "herdr.sock"))

	result, err := client.TabCreate(context.Background(), "workspace-1", "/home/user/project", "agent")
	if err != nil {
		t.Fatalf("TabCreate() error = %v", err)
	}
	if result.PaneID != "pane-2" || result.TabID != "tab-2" || result.WorkspaceID != "workspace-1" {
		t.Fatalf("TabCreate() = %#v, want nested root-pane identifiers", result)
	}
}

func TestCreateResultKeepsFlatCompatibility(t *testing.T) {
	bin := writeResultScript(t, `{"result":{"pane_id":"pane-flat","tab_id":"tab-flat","workspace_id":"workspace-flat"}}`)
	client := NewClient(bin, filepath.Join(t.TempDir(), "herdr.sock"))

	result, err := client.TabCreate(context.Background(), "workspace-flat", "/home/user/project", "agent")
	if err != nil {
		t.Fatalf("TabCreate() error = %v", err)
	}
	if result.PaneID != "pane-flat" || result.TabID != "tab-flat" || result.WorkspaceID != "workspace-flat" {
		t.Fatalf("TabCreate() = %#v, want flat identifiers", result)
	}
}

func TestStartAgentParsesHerdr075NestedResult(t *testing.T) {
	bin := writeResultScript(t, `{"result":{"type":"agent_started","agent":{"pane_id":"pane-result","agent":"codex","name":"project-codex","running":true}}}`)
	client := NewClient(bin, filepath.Join(t.TempDir(), "herdr.sock"))

	paneID, err := client.StartAgent(context.Background(), "project-codex", "codex", "pane-request", 30_000)
	if err != nil {
		t.Fatalf("StartAgent() error = %v", err)
	}
	if paneID != "pane-result" {
		t.Fatalf("StartAgent() pane_id = %q, want nested agent pane-result", paneID)
	}
}

func TestCreateWithoutRootPaneIsUnsafeToRetry(t *testing.T) {
	bin := writeResultScript(t, `{"result":{"type":"tab_created","tab":{"tab_id":"tab-2","workspace_id":"workspace-1"}}}`)
	client := NewClient(bin, filepath.Join(t.TempDir(), "herdr.sock"))

	_, err := client.TabCreate(context.Background(), "workspace-1", "/home/user/project", "agent")
	if !errors.Is(err, ErrCreatedTargetUnknown) {
		t.Fatalf("TabCreate() error = %v, want ErrCreatedTargetUnknown", err)
	}
	if !errors.Is(err, ErrDispatchedUnknown) {
		t.Fatalf("TabCreate() error = %v, want ErrDispatchedUnknown", err)
	}
	if errors.Is(err, ErrNotStarted) {
		t.Fatalf("TabCreate() error = %v, must not be safe to retry", err)
	}
}
