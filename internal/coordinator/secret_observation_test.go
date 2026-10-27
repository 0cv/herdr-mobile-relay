package coordinator

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func secretReadSocket(t *testing.T, content string, fail bool, afterRead func()) string {
	t.Helper()
	path := filepath.Join(testSocketDir(t), "secret.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	var clients sync.Map
	var workers sync.WaitGroup
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			clients.Store(conn, true)
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer clients.Delete(conn)
				defer conn.Close()
				decoder := json.NewDecoder(conn)
				for {
					var request struct {
						ID     string `json:"id"`
						Method string `json:"method"`
					}
					if decoder.Decode(&request) != nil {
						return
					}
					response := map[string]any{"id": request.ID}
					if request.Method != "pane.read" || fail {
						response["error"] = map[string]any{"code": "read_failed", "message": "controlled read failure"}
					} else {
						response["result"] = map[string]any{"type": "pane_read", "read": map[string]any{"text": content}}
						if afterRead != nil {
							afterRead()
						}
					}
					if json.NewEncoder(conn).Encode(response) != nil {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		<-done
		clients.Range(func(key, value any) bool { key.(net.Conn).Close(); return true })
		workers.Wait()
	})
	return path
}

func TestSecretRechecksPromptBeforeAnyWrite(t *testing.T) {
	for _, scenario := range []string{"echo-on", "unknown", "read failure", "replacement", "valid"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			record := filepath.Join(dir, "writes")
			bin := recordingSecretHerdr(t, dir, record)
			state := NewState(testLogger())
			state.CommitInventory([]*AgentState{{PaneID: "pane-1", Agent: "codex", Status: "working"}}, state.RevisionCounter())
			content := "Password:"
			if scenario == "echo-on" {
				content = "Password accepted\n$ "
			}
			if scenario == "unknown" {
				content = ""
			}
			var afterRead func()
			if scenario == "replacement" {
				afterRead = func() { state.BumpGeneration("pane-1") }
			}
			client := herdr.NewClient(bin, secretReadSocket(t, content, scenario == "read failure", afterRead))
			defer client.Close()
			d := NewDispatcher(client, state, nil, testLogger())
			defer d.Close(context.Background())
			result := d.Handle(context.Background(), map[string]any{"action": "send_secret", "pane_id": "pane-1", "request_id": "observed-secret", "text": "synthetic-value"})
			if scenario == "valid" {
				if !result.OK || len(recordedInvocations(t, record)) != 1 {
					t.Fatalf("valid dispatch: %+v", result)
				}
			} else {
				if result.OK {
					t.Fatalf("unverified secret dispatched: %+v", result)
				}
				if _, err := os.Stat(record); err == nil {
					for _, invocation := range recordedInvocations(t, record) {
						if len(invocation) > 1 && (invocation[1] == "send-keys" || invocation[1] == "send-text") {
							t.Fatal("unverified secret reached the terminal")
						}
					}
				} else if !os.IsNotExist(err) {
					t.Fatal(err)
				}
			}
		})
	}
}
