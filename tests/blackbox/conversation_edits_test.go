package blackbox

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func pairPhone(t *testing.T, ctx context.Context, env *hybridEnv, invite phoneAuth, role string) (*phone, string) {
	t.Helper()
	pending := dialPhoneAs(t, env, invite)
	finish := pending.awaitMessage(ctx, "e2ee_server_finish")
	secret, err := base64.RawURLEncoding.DecodeString(finish["credential_secret"].(string))
	if err != nil || finish["role"] != role {
		t.Fatalf("could not enroll %s: %v", role, err)
	}
	_ = pending.conn.CloseNow()
	return dialPhoneAs(t, env, phoneAuth{"credential", finish["credential_id"].(string), uint64(finish["credential_version"].(float64)), secret}), finish["device_id"].(string)
}

func TestPairedReadersCanReviewConversationEdits(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(home, ".claude", "projects", "-fixture")
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	const session = "123e4567-e89b-12d3-a456-426614174000"
	path := filepath.Join(project, session+".jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"assistant","uuid":"edit","message":{"content":[{"type":"tool_use","id":"call-edit","name":"Edit","input":{"file_path":"src/main.go","old_string":"before","new_string":"after"}}]}}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	const scenario = `{"panes":[{"pane_id":"pane-1","terminal_id":"terminal-1","agent":"claude","name":"fixture","agent_status":"working","tab_id":"tab-1","workspace_id":"ws-1","cwd":"/fixture","foreground_cwd":"/fixture","agent_session":{"kind":"id","value":"` + session + `"}}],"tabs":[{"tab_id":"tab-1","workspace_id":"ws-1","label":"fixture","number":1,"cwd":"/fixture"}]}`
	env := setupHybridEnv(t, relayOptions{scenario: scenario, env: []string{"HOME=" + home, "HERDR_CLAUDE_CONFIG_DIRS=" + filepath.Join(home, ".claude")}})
	waitForRegistration(t, env.relayHTTP)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	controller, _ := pairPhone(t, ctx, env, phoneAuth{"invitation", "bootstrap", 1, []byte(relayKey)}, "controller")
	controller.awaitMessage(ctx, "push_config")
	controller.sendMessage(ctx, map[string]any{"type": "create_device_invitation", "protocol": 3, "request_id": "invite", "role": "reader", "name": "Fixture reader"})
	result := controller.awaitMessage(ctx, "command_result")
	if result["ok"] != true {
		t.Fatalf("could not create reader invitation: %+v", result)
	}
	invitation := result["data"].(map[string]any)["invitation"].(map[string]any)
	secret, err := base64.RawURLEncoding.DecodeString(invitation["secret"].(string))
	if err != nil {
		t.Fatal(err)
	}
	reader, device := pairPhone(t, ctx, env, phoneAuth{"invitation", invitation["invitation_id"].(string), uint64(invitation["version"].(float64)), secret}, "reader")
	reader.awaitMessage(ctx, "push_config")
	reader.sendMessage(ctx, map[string]any{"type": "refresh_agents"})
	var target map[string]any
	for target == nil {
		agents := reader.awaitMessage(ctx, "agents")["agents"].([]any)
		if len(agents) != 0 {
			agent := agents[0].(map[string]any)
			target = map[string]any{"pane_id": agent["pane_id"], "terminal_id": agent["terminal_id"], "generation": agent["generation"], "server_session_id": agent["server_session_id"], "agent_session_id": session}
		}
	}
	request := map[string]any{"type": "get_conversation_history", "protocol": 3, "request_id": "review", "pane_id": "pane-1", "target": target, "observe": true}
	read := func(p *phone) (diff any, page map[string]any) {
		p.sendMessage(ctx, request)
		for {
			response := p.readMessage(ctx)
			if response["request_id"] != "review" {
				continue
			}
			if response["ok"] != true {
				t.Fatalf("history read failed: %+v", response)
			}
			page = response["data"].(map[string]any)
			return page["entries"].([]any)[0].(map[string]any)["tools"].([]any)[0].(map[string]any)["diff"], page
		}
	}
	controllerDiff, _ := read(controller)
	readerDiff, observed := read(reader)
	if readerDiff == nil || !reflect.DeepEqual(readerDiff, controllerDiff) || readerDiff.(map[string]any)["text"] != "-before\n+after" {
		t.Fatalf("reader diff = %v, controller diff = %v", readerDiff, controllerDiff)
	}
	reader.sendMessage(ctx, map[string]any{"type": "submit_prompt", "protocol": 3, "request_id": "mutate", "pane_id": "pane-1", "target": target, "text": "must not send"})
	if denied := reader.awaitMessage(ctx, "error"); denied["request_id"] != "mutate" || denied["error"].(map[string]any)["code"] != "reader_denied" {
		t.Fatalf("reader mutation result = %v", denied)
	}
	stale := maps.Clone(target)
	stale["generation"] = target["generation"].(float64) + 1
	staleRequest := maps.Clone(request)
	staleRequest["request_id"], staleRequest["target"] = "stale", stale
	reader.sendMessage(ctx, staleRequest)
	if rejected := reader.awaitMessage(ctx, "error"); rejected["request_id"] != "stale" || rejected["error"].(map[string]any)["code"] != "invalid_request" {
		t.Fatalf("stale target result = %v", rejected)
	}
	reader.sendMessage(ctx, map[string]any{"type": "get_conversation_updates", "protocol": 3, "request_id": "pending", "pane_id": "pane-1", "target": target, "subscription_id": observed["subscription_id"], "source_revision": observed["source_revision"], "after": observed["sequence"]})
	controller.sendMessage(ctx, map[string]any{"type": "revoke_device", "protocol": 3, "request_id": "revoke", "device_id": device})
	if revoked := controller.awaitMessage(ctx, "command_result"); revoked["request_id"] != "revoke" || revoked["ok"] != true {
		t.Fatalf("revoke result = %v", revoked)
	}
	for {
		_, frame, err := reader.conn.Read(ctx)
		if websocket.CloseStatus(err) != -1 {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		raw, err := reader.assembly.Push(frame)
		if err != nil {
			t.Fatal(err)
		}
		if raw == nil {
			continue
		}
		var response map[string]any
		if err := json.Unmarshal(reader.open(raw), &response); err != nil {
			t.Fatal(err)
		}
		if response["request_id"] == "pending" {
			if response["ok"] == true {
				t.Fatalf("revoked reader received updates: %v", response)
			}
			break
		}
	}
}
