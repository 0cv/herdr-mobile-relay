package herdr

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
)

func TestPaneGetReturnsTerminalIdentity(t *testing.T) {
	socketPath, done := startUnaryTestSocket(t, "pane.get", map[string]any{
		"type": "pane_info",
		"pane": map[string]any{"pane_id": "w1:p2", "terminal_id": "terminal-7", "workspace_id": "w1", "tab_id": "w1:t1"},
	})
	client := NewClient("", socketPath)
	pane, err := client.PaneGet(context.Background(), "w1:p2")
	if err != nil {
		t.Fatalf("PaneGet() error = %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if pane.ID != "w1:p2" || pane.TerminalID != "terminal-7" {
		t.Fatalf("PaneGet() = %#v", pane)
	}
}

func TestPaneProcessInfoFreshUsesSocket(t *testing.T) {
	socketPath, done := startUnaryTestSocket(t, "pane.process_info", map[string]any{
		"type": "pane_process_info",
		"process_info": map[string]any{
			"pane_id": "w1:p2", "shell_pid": 101, "foreground_process_group_id": 202,
			"foreground_processes": []any{map[string]any{"pid": 202, "name": "vim"}},
		},
	})
	client := NewClient("", socketPath)
	info, err := client.PaneProcessInfoFresh(context.Background(), "w1:p2")
	if err != nil {
		t.Fatalf("PaneProcessInfoFresh() error = %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if info.PaneID != "w1:p2" || info.ShellPID != 101 || info.ForegroundProcessGroupID != 202 {
		t.Fatalf("PaneProcessInfoFresh() = %#v", info)
	}
}

func TestPaneGetSurfacesPaneNotFound(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "herdr.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		var request struct {
			ID string `json:"id"`
		}
		if json.NewDecoder(conn).Decode(&request) != nil {
			return
		}
		_ = json.NewEncoder(conn).Encode(map[string]any{
			"id":    request.ID,
			"error": map[string]any{"code": "pane_not_found", "message": "pane w1:p2 not found"},
		})
	}()
	_, err = NewClient("", socketPath).PaneGet(context.Background(), "w1:p2")
	if !IsPaneNotFound(err) {
		t.Fatalf("PaneGet() error = %v, want pane_not_found", err)
	}
	if IsPaneNotFound(context.DeadlineExceeded) {
		t.Fatal("a timeout was classified as pane_not_found")
	}
}
