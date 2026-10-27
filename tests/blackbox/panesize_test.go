package blackbox

import (
	"context"
	"encoding/json"
	"runtime"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Exercise the compiled relay's real Run dispatch, not a mirrored test handler.
// Stale clients (apps up to 0.22.7) lease without opt_in and may send either
// wire envelope with obsolete client identity fields: none of that may reach
// a terminal-size backend. An opt-in lease reaches the manager, which fails
// cleanly here because the fake Herdr has no pane.get or pane.process_info.
func TestPaneSizeCommandsThroughServer(t *testing.T) {
	env := setupEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, env.wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	supported := runtime.GOOS == "darwin" || runtime.GOOS == "linux"
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read handshake: %v", err)
		}
		var message struct {
			Type         string   `json:"type"`
			Capabilities []string `json:"capabilities"`
		}
		if json.Unmarshal(data, &message) != nil || message.Type != "push_config" {
			continue
		}
		advertised := map[string]bool{}
		for _, capability := range message.Capabilities {
			advertised[capability] = true
		}
		if advertised["pane_size_lease"] || advertised["pane_size_lease_rows"] {
			t.Fatalf("relay advertised a legacy auto-lease capability: %v", message.Capabilities)
		}
		if advertised["pane_size_opt_in"] != supported || advertised["pane_size_opt_in_rows"] != supported {
			t.Fatalf("opt-in capabilities on %s: %v", runtime.GOOS, message.Capabilities)
		}
		break
	}

	optInFailure := "Pane terminal identity is unavailable"
	if !supported {
		optInFailure = "Resize Session is unavailable on this relay"
	}
	for _, envelope := range []string{"direct", "command"} {
		t.Run(envelope, func(t *testing.T) {
			for _, command := range []struct {
				id     string
				action string
				fields map[string]any
				ok     bool
				phase  string
				error  string
			}{
				{"acquire-without-opt-in", "lease_pane_size", nil, false, "failed", "Resize Session is unavailable on this relay"},
				{"renew-without-opt-in", "lease_pane_size", nil, false, "failed", "Resize Session is unavailable on this relay"},
				{"opt-in-false", "lease_pane_size", map[string]any{"opt_in": false}, false, "failed", "Resize Session is unavailable on this relay"},
				{"opt-in", "lease_pane_size", map[string]any{"opt_in": true}, false, "failed", optInFailure},
				{"release", "release_pane_size", nil, true, "completed", ""},
				{"release-immediate", "release_pane_size", map[string]any{"immediate": true}, true, "completed", ""},
				{"release-again", "release_pane_size", nil, true, "completed", ""},
			} {
				t.Run(command.id, func(t *testing.T) {
					requestID := envelope + "-" + command.id
					request := map[string]any{
						"type":       command.action,
						"request_id": requestID,
						"protocol":   3,
						"pane_id":    "pane-1",
						"client_id":  "legacy-client",
						"columns":    80,
						"rows":       24,
					}
					for key, value := range command.fields {
						request[key] = value
					}
					if envelope == "command" {
						request["type"] = "command"
						request["action"] = command.action
					}
					data, err := json.Marshal(request)
					if err != nil {
						t.Fatal(err)
					}
					if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
						t.Fatalf("write: %v", err)
					}
					result := readJSON(t, conn, ctx, 10*time.Second)
					if result["type"] != "command_result" || result["request_id"] != requestID ||
						result["action"] != command.action || result["pane_id"] != "pane-1" {
						t.Fatalf("wrong response envelope: %v", result)
					}
					if result["ok"] != command.ok || result["phase"] != command.phase || result["error"] != command.error {
						t.Fatalf("result = %v, want ok=%v phase=%q error=%q", result, command.ok, command.phase, command.error)
					}
				})
			}
		})
	}
	if env.relayCmd.ProcessState != nil {
		t.Fatalf("relay exited: %v", env.relayCmd.ProcessState)
	}
	waitForStatus(t, env.httpBase, "/healthz", 200)
}
