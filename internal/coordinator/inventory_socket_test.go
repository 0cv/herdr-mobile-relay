package coordinator

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func testSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "s-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	return dir
}

func TestSocketFixtureDirectoryUsesSelectedTempAndCleanup(t *testing.T) {
	var directory string
	t.Run(strings.Repeat("long-test-name-", 8), func(t *testing.T) {
		directory = testSocketDir(t)
		if filepath.Dir(directory) != os.TempDir() {
			t.Fatalf("socket directory %q is outside the selected temporary directory", directory)
		}
		info, err := os.Lstat(directory)
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("socket directory mode = %s", info.Mode())
		}
	})
	if _, err := os.Lstat(directory); !os.IsNotExist(err) {
		t.Fatalf("socket directory was not cleaned up: %v", err)
	}
}

func startInventorySocket(t *testing.T, socketPath string, workspaces []any, panes ...herdr.Pane) {
	t.Helper()
	if workspaces == nil {
		workspaces = []any{}
	}
	if panes == nil {
		panes = []herdr.Pane{}
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen inventory socket: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			func() {
				defer conn.Close()
				var request struct {
					ID     string `json:"id"`
					Method string `json:"method"`
				}
				if json.NewDecoder(bufio.NewReader(conn)).Decode(&request) != nil {
					return
				}
				result := map[string]any{"type": "agent_list", "agents": []any{}}
				switch request.Method {
				case "workspace.list":
					result = map[string]any{"type": "workspace_list", "workspaces": workspaces}
				case "pane.list":
					result = map[string]any{"type": "pane_list", "panes": panes}
				case "workspace.close":
					result = map[string]any{"type": "ok"}
				case "agent.list":
				default:
					return
				}
				_ = json.NewEncoder(conn).Encode(map[string]any{"id": request.ID, "result": result})
			}()
		}
	}()
}
