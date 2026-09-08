package coordinator

import (
	"bufio"
	"encoding/json"
	"net"
	"testing"
)

func startInventorySocket(t *testing.T, socketPath string, workspaces []any) {
	t.Helper()
	if workspaces == nil {
		workspaces = []any{}
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
					result = map[string]any{"type": "pane_list", "panes": []any{}}
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
