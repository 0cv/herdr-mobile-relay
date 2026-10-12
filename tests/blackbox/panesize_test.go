package blackbox

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Exercise the compiled relay's real Run dispatch, not a mirrored test handler.
// Stale clients may send either wire envelope and include obsolete geometry or
// client identity fields. Neither form may reach a terminal-size backend.
func TestLegacyPaneSizeCommandsThroughServer(t *testing.T) {
	env := setupEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, env.wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	for _, envelope := range []string{"direct", "command"} {
		t.Run(envelope, func(t *testing.T) {
			for _, command := range []struct {
				id     string
				action string
				ok     bool
				phase  string
				error  string
			}{
				{"acquire", "lease_pane_size", false, "failed", "Resize Session is unavailable on this relay"},
				{"renew", "lease_pane_size", false, "failed", "Resize Session is unavailable on this relay"},
				{"release", "release_pane_size", true, "completed", ""},
				{"release-again", "release_pane_size", true, "completed", ""},
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
					result := readJSON(t, conn, ctx, 5*time.Second)
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
}
