package blackbox

import (
	"context"
	"encoding/base64"
	"testing"
	"time"
)

func TestPairedPaneReadsAcknowledgeOnlyForControllers(t *testing.T) {
	const scenario = `{"panes":[{"pane_id":"pane-1","terminal_id":"terminal-1","agent":"codex","agent_status":"succeeded","tab_id":"tab-1","workspace_id":"ws-1"}],
	"tabs":[{"tab_id":"tab-1","workspace_id":"ws-1","label":"fixture","number":1}],"content":{"pane-1":"Fixture result"}}`
	for _, action := range []string{"read_pane", "watch_pane"} {
		t.Run(action, func(t *testing.T) {
			env := setupHybridEnv(t, relayOptions{scenario: scenario, env: []string{"HOME=" + t.TempDir()}})
			waitForRegistration(t, env.relayHTTP)
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()

			controller, _ := pairPhone(t, ctx, env, phoneAuth{"invitation", "bootstrap", 1, []byte(relayKey)}, "controller")
			snapshot := func() map[string]any {
				agents := controller.awaitMessage(ctx, "agents")["agents"].([]any)
				if len(agents) != 1 {
					t.Fatalf("agents = %v, want one result", agents)
				}
				return agents[0].(map[string]any)
			}
			agent := snapshot()
			if agent["status"] != "succeeded" {
				t.Fatalf("initial result = %v", agent)
			}
			target := map[string]any{"pane_id": agent["pane_id"], "terminal_id": agent["terminal_id"], "generation": agent["generation"], "server_session_id": agent["server_session_id"]}
			controller.sendMessage(ctx, map[string]any{"type": "create_device_invitation", "protocol": 3, "request_id": "invite", "role": "reader", "name": "Fixture reader"})
			result := controller.awaitMessage(ctx, "command_result")
			if result["ok"] != true {
				t.Fatalf("could not create reader invitation: %v", result)
			}
			invite := result["data"].(map[string]any)["invitation"].(map[string]any)
			secret, err := base64.RawURLEncoding.DecodeString(invite["secret"].(string))
			if err != nil {
				t.Fatal(err)
			}
			reader, _ := pairPhone(t, ctx, env, phoneAuth{"invitation", invite["invitation_id"].(string), uint64(invite["version"].(float64)), secret}, "reader")

			reader.sendMessage(ctx, map[string]any{"type": "acknowledge_pane", "protocol": 3, "request_id": "denied", "pane_id": "pane-1", "target": target})
			if denied := reader.awaitMessage(ctx, "error"); denied["request_id"] != "denied" || denied["error"].(map[string]any)["code"] != "reader_denied" {
				t.Fatalf("reader acknowledgement result = %v", denied)
			}
			read := func(p *phone) {
				p.sendMessage(ctx, map[string]any{"type": action, "protocol": 3, "request_id": action, "pane_id": "pane-1", "target": target})
				frame := p.awaitMessage(ctx, "pane_content")
				if frame["pane_id"] != "pane-1" || frame["content"] != "Fixture result\n" {
					t.Fatalf("pane response = %v", frame)
				}
			}
			read(reader)
			controller.sendMessage(ctx, map[string]any{"type": "refresh_agents"})
			if after := snapshot(); after["status"] != "succeeded" || after["last_seen_at"] != agent["last_seen_at"] {
				t.Fatalf("reader %s changed the result: %v", action, after)
			}
			read(controller)
			reader.awaitMatchingMessage(ctx, "an idle status for pane-1 after controller "+action, func(message map[string]any) bool {
				switch message["type"] {
				case "agent_update":
					return message["pane_id"] == "pane-1" && message["status"] == "idle"
				case "agents":
					agents, _ := message["agents"].([]any)
					for _, entry := range agents {
						agent, _ := entry.(map[string]any)
						if agent["pane_id"] == "pane-1" && agent["status"] == "idle" {
							return true
						}
					}
				}
				return false
			})
		})
	}
}
