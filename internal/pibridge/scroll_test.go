package pibridge

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func scrollEndpoint(t *testing.T, change func(map[string]any), delay time.Duration) (string, Identity) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "pi-scroll-")
	if err != nil {
		t.Fatal(err)
	}
	id := Identity{Instance: strings.Repeat("a", 64), Pane: "pane", Session: "session", PID: os.Getpid()}
	path := filepath.Join(dir, fmtPID(id.PID)+".sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() { _ = listener.Close(); <-done; _ = os.RemoveAll(dir) })
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		var request struct {
			Identity
			Operation string `json:"operation"`
			Challenge string `json:"challenge"`
		}
		if json.NewDecoder(conn).Decode(&request) != nil {
			return
		}
		if request.Operation != "scroll_state" {
			return
		}
		time.Sleep(delay)
		result := map[string]any{
			"instance": request.Instance, "pane": request.Pane, "session": request.Session, "pid": request.PID,
			"challenge": request.Challenge, "incarnation": strings.Repeat("b", 36), "revision": strings.Repeat("c", 64),
			"renderer": "fullscreen", "wheel": "sgr_cell", "reason": "pi_fullscreen",
		}
		if change != nil {
			change(result)
		}
		_ = json.NewEncoder(conn).Encode(result)
	}()
	return dir, id
}

func TestScrollQuery(t *testing.T) {
	for _, values := range [][3]string{{"fullscreen", "sgr_cell", "pi_fullscreen"}, {"regular", "unavailable", "pi_regular"}, {"unknown", "unavailable", "ui_unavailable"}} {
		t.Run(values[0], func(t *testing.T) {
			dir, id := scrollEndpoint(t, func(result map[string]any) {
				result["renderer"], result["wheel"], result["reason"] = values[0], values[1], values[2]
			}, 0)
			result, err := queryScrollState(context.Background(), dir, id)
			if err != nil || result.Renderer != values[0] || result.Wheel != values[1] || result.Identity != id {
				t.Fatalf("scroll state: %+v, %v", result, err)
			}
		})
	}
}

func TestScrollQueryStrictDecodingAndBinding(t *testing.T) {
	for _, field := range []string{"instance", "pane", "session", "pid", "challenge", "incarnation", "revision", "renderer", "wheel", "reason", "commands"} {
		t.Run(field, func(t *testing.T) {
			dir, id := scrollEndpoint(t, func(result map[string]any) { result[field] = "invalid" }, 0)
			if _, err := queryScrollState(context.Background(), dir, id); err == nil {
				t.Fatal("accepted invalid scroll response")
			}
		})
	}
	for _, field := range []string{"renderer", "wheel", "reason"} {
		t.Run("missing-"+field, func(t *testing.T) {
			dir, id := scrollEndpoint(t, func(result map[string]any) { delete(result, field) }, 0)
			if _, err := queryScrollState(context.Background(), dir, id); err == nil {
				t.Fatal("accepted missing scroll field")
			}
		})
	}
	dir, id := scrollEndpoint(t, func(result map[string]any) { result["renderer"] = "regular" }, 0)
	if _, err := queryScrollState(context.Background(), dir, id); err == nil {
		t.Fatal("regular renderer authorized SGR wheels")
	}
	dir, id = scrollEndpoint(t, func(result map[string]any) { result["extra"] = strings.Repeat("x", 4096) }, 0)
	if _, err := queryScrollState(context.Background(), dir, id); err == nil {
		t.Fatal("accepted oversized scroll query")
	}
}

func TestScrollQueryDeadlineUnavailableAndPeer(t *testing.T) {
	dir, id := scrollEndpoint(t, nil, 50*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := queryScrollState(ctx, dir, id); err == nil {
		t.Fatal("accepted late bridge evidence")
	}
	if _, err := QueryScrollState(context.Background(), Identity{}); err == nil {
		t.Fatal("accepted missing bridge")
	}
	dir, id = scrollEndpoint(t, nil, 0)
	original := filepath.Join(dir, fmtPID(id.PID)+".sock")
	id.PID++
	if err := os.Rename(original, filepath.Join(dir, fmtPID(id.PID)+".sock")); err != nil {
		t.Fatal(err)
	}
	if _, err := queryScrollState(context.Background(), dir, id); err == nil {
		t.Fatal("accepted forged owner")
	}
}
