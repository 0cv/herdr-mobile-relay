package blackbox

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestSendPromptAndReceiveResult(t *testing.T) {
	env := setupEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, env.wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	// Send a prompt command
	prompt := map[string]any{
		"type":       "command",
		"action":     "submit_prompt",
		"request_id": "test-req-1",
		"protocol":   2,
		"pane_id":    "pane-1",
		"text":       "Hello world",
	}
	data, _ := json.Marshal(prompt)
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Expect command_result
	msg := readJSON(t, conn, ctx, 5*time.Second)
	if msg["type"] != "command_result" {
		t.Fatalf("expected command_result, got %v", msg["type"])
	}
	if msg["request_id"] != "test-req-1" {
		t.Errorf("request_id = %v", msg["request_id"])
	}
	if msg["action"] != "prompt" {
		t.Errorf("action = %v", msg["action"])
	}
	if msg["ok"] != true {
		t.Errorf("ok = %v, want true", msg["ok"])
	}
	if msg["phase"] != "completed" {
		t.Errorf("phase = %v", msg["phase"])
	}
}

func TestSendKeysCommand(t *testing.T) {
	env := setupEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, env.wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	cmd := map[string]any{
		"type":       "command",
		"action":     "send_keys",
		"request_id": "test-req-2",
		"protocol":   2,
		"pane_id":    "pane-1",
		"keys":       []string{"Enter"},
	}
	data, _ := json.Marshal(cmd)
	conn.Write(ctx, websocket.MessageText, data)

	msg := readJSON(t, conn, ctx, 5*time.Second)
	if msg["type"] != "command_result" {
		t.Fatalf("expected command_result, got %v", msg["type"])
	}
	if msg["ok"] != true {
		t.Errorf("ok = %v, error = %v", msg["ok"], msg["error"])
	}
	if msg["action"] != "keys" {
		t.Errorf("action = %v", msg["action"])
	}
}

func TestAcknowledgePane(t *testing.T) {
	env := setupEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, env.wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	cmd := map[string]any{
		"type":       "command",
		"action":     "acknowledge_pane",
		"request_id": "test-req-3",
		"protocol":   2,
		"pane_id":    "pane-1",
	}
	data, _ := json.Marshal(cmd)
	conn.Write(ctx, websocket.MessageText, data)

	msg := readJSON(t, conn, ctx, 5*time.Second)
	if msg["type"] != "command_result" {
		t.Fatalf("expected command_result, got %v", msg["type"])
	}
	if msg["ok"] != true {
		t.Errorf("ok = %v", msg["ok"])
	}
	if msg["action"] != "acknowledge_pane" {
		t.Errorf("action = %v", msg["action"])
	}
}

func TestUnknownActionReturnsError(t *testing.T) {
	env := setupEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, env.wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	cmd := map[string]any{
		"type":       "command",
		"action":     "nonexistent_action",
		"request_id": "test-req-4",
		"protocol":   2,
	}
	data, _ := json.Marshal(cmd)
	conn.Write(ctx, websocket.MessageText, data)

	msg := readJSON(t, conn, ctx, 5*time.Second)
	if msg["type"] != "command_result" {
		t.Fatalf("expected command_result, got %v", msg["type"])
	}
	if msg["ok"] != false {
		t.Errorf("ok = %v, want false", msg["ok"])
	}
	if msg["phase"] != "failed" {
		t.Errorf("phase = %v", msg["phase"])
	}
}

func TestReadPane(t *testing.T) {
	env := setupEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, env.wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	cmd := map[string]any{
		"type":    "read_pane",
		"pane_id": "pane-1",
		"lines":   10,
		"format":  "text",
	}
	data, _ := json.Marshal(cmd)
	conn.Write(ctx, websocket.MessageText, data)

	msg := readJSON(t, conn, ctx, 5*time.Second)
	if msg["type"] != "pane_content" {
		t.Fatalf("expected pane_content, got %v", msg["type"])
	}
	if msg["pane_id"] != "pane-1" {
		t.Errorf("pane_id = %v", msg["pane_id"])
	}
	content, _ := msg["content"].(string)
	if content == "" {
		t.Error("content is empty")
	}
}

func TestReadAndAnswerStructuredQuestion(t *testing.T) {
	questionView := "Which deployment target?\n❯ 1. Development\n2. Staging\n3. Type something.\n4. Chat about this\nEnter to select · ↑/↓ to navigate · Esc to cancel"
	scenario, err := json.Marshal(map[string]any{
		"panes": []map[string]any{{
			"pane_id": "pane-1", "agent": "claude", "name": "test",
			"agent_status": "blocked", "tab_id": "tab-1",
			"workspace_id": "ws-1", "cwd": "/tmp", "revision": 1,
		}},
		"tabs": []map[string]any{{
			"tab_id": "tab-1", "workspace_id": "ws-1", "label": "main", "number": 1, "cwd": "/tmp",
		}},
		"content": map[string]string{"pane-1": questionView},
	})
	if err != nil {
		t.Fatal(err)
	}
	env := setupEnvWithScenario(t, string(scenario))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, env.wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	read, _ := json.Marshal(map[string]any{
		"type": "read_pane", "pane_id": "pane-1", "lines": 80, "format": "ansi",
	})
	if err := conn.Write(ctx, websocket.MessageText, read); err != nil {
		t.Fatal(err)
	}
	content := readJSON(t, conn, ctx, 5*time.Second)
	interaction, ok := content["interaction"].(map[string]any)
	if !ok || interaction["id"] == "" || interaction["question"] != "Which deployment target?" {
		t.Fatalf("interaction = %+v", content["interaction"])
	}

	answer, _ := json.Marshal(map[string]any{
		"type": "answer_question", "protocol": 2, "request_id": "question-1",
		"pane_id": "pane-1", "interaction_id": interaction["id"],
		"selected_indices": []int{1}, "other_selected": false, "other_text": "",
	})
	if err := conn.Write(ctx, websocket.MessageText, answer); err != nil {
		t.Fatal(err)
	}
	result := readJSON(t, conn, ctx, 5*time.Second)
	if result["type"] != "command_result" || result["ok"] != true || result["phase"] != "accepted" {
		t.Fatalf("question result = %+v", result)
	}
}

func readJSON(t *testing.T, conn *websocket.Conn, ctx context.Context, timeout time.Duration) map[string]any {
	t.Helper()
	readCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		_, data, err := conn.Read(readCtx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var msg map[string]any
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		// Skip broadcasts and handshake messages, wait for the response we care about
		switch msg["type"] {
		case "agents", "agent_update", "activity", "push_config", "activity_history", "inventory_status":
			continue
		}
		return msg
	}
}
