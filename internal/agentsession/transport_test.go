package agentsession

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestReportTransport(t *testing.T) {
	for _, mode := range []string{"success", "get error", "unrelated pid", "no foreground", "label error", "bad hello"} {
		t.Run(mode, func(t *testing.T) {
			root, err := os.MkdirTemp("/tmp", "as-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(root)
			socket := filepath.Join(root, "core.sock")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("HC_TERMINAL_ID", "t1")
			t.Setenv("HC_TERMINAL_SOCKET", socket)
			var mu sync.Mutex
			var labels map[string]string
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					conn, err := listener.Accept()
					if err != nil {
						return
					}
					reader := bufio.NewReader(conn)
					for {
						line, err := reader.ReadBytes('\n')
						if err != nil {
							break
						}
						var req struct {
							ID, Method string
							Params     struct {
								Terminal string
								Labels   map[string]string
							}
						}
						if json.Unmarshal(line, &req) != nil {
							break
						}
						var result any = map[string]any{}
						response := map[string]any{"jsonrpc": "2.0", "id": req.ID}
						switch req.Method {
						case "hello":
							generation := 1
							if mode == "bad hello" {
								generation = 2
							}
							result = map[string]any{"generation": generation}
						case "terminal.get":
							pid := os.Getppid()
							if mode == "unrelated pid" {
								pid = os.Getpid()
							}
							term := map[string]any{"address": "t1", "pid": pid, "state": "live"}
							if mode != "no foreground" {
								term["foreground"] = map[string]int{"pid": pid}
							}
							result = term
							if mode == "get error" {
								response["error"] = map[string]any{"code": -32602, "message": "refused"}
							}
						case "terminal.label":
							mu.Lock()
							labels = req.Params.Labels
							mu.Unlock()
							if mode == "label error" {
								response["error"] = map[string]any{"code": -32602, "message": "refused"}
							}
						}
						if response["error"] == nil {
							response["result"] = result
						}
						if json.NewEncoder(conn).Encode(response) != nil {
							break
						}
					}
					conn.Close()
				}
			}()
			Run("codex", strings.NewReader(`{"session_id":"abc","transcript_path":"/tmp/log"}`))
			listener.Close()
			wg.Wait()
			mu.Lock()
			defer mu.Unlock()
			if mode == "success" || mode == "label error" {
				if len(labels) != 2 || labels["herdr-mobile-relay.session"] != "abc" || labels["herdr-mobile-relay.pid"] != strconv.Itoa(os.Getppid()) {
					t.Fatalf("labels: %v", labels)
				}
			} else if labels != nil {
				t.Fatalf("unexpected labels: %v", labels)
			}
		})
	}
}
