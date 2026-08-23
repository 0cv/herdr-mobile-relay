package blackbox

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
	if operation := findFakeOperation(t, env.operationsLog, "agent", "prompt", "pane-1", "Hello world"); operation == nil {
		t.Fatal("non-Qoder prompt did not use Herdr agent prompt")
	}
	if operation := findFakeOperation(t, env.operationsLog, "pane", "send-keys", "pane-1", "Enter"); operation != nil {
		t.Fatalf("non-Qoder prompt sent an extra Enter key: %v", operation)
	}

}

func TestQoderPromptSendsTextAndEnter(t *testing.T) {
	scenario := `{"panes":[{"pane_id":"pane-1","agent":"qodercli","name":"test","agent_status":"working","tab_id":"tab-1","workspace_id":"ws-1","cwd":"/tmp","revision":1,"foreground_cwd":"/tmp"}],"tabs":[{"tab_id":"tab-1","workspace_id":"ws-1","label":"main","number":1,"cwd":"/tmp"}]}`
	env := setupEnvWithScenario(t, scenario)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, env.wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	prompt, _ := json.Marshal(map[string]any{
		"type":       "command",
		"action":     "submit_prompt",
		"request_id": "qoder-prompt",
		"protocol":   2,
		"pane_id":    "pane-1",
		"text":       "/permissions",
	})
	if err := conn.Write(ctx, websocket.MessageText, prompt); err != nil {
		t.Fatalf("write: %v", err)
	}
	result := readJSON(t, conn, ctx, 5*time.Second)
	if result["ok"] != true || result["phase"] != "completed" {
		t.Fatalf("Qoder prompt result = %v", result)
	}
	if operation := findFakeOperation(t, env.operationsLog, "pane", "send-text", "pane-1", "/permissions"); operation == nil {
		t.Fatal("Qoder prompt did not insert its text")
	}
	if operation := findFakeOperation(t, env.operationsLog, "pane", "send-keys", "pane-1", "Enter"); operation == nil {
		t.Fatal("Qoder prompt did not submit with Enter")
	}
	if operation := findFakeOperation(t, env.operationsLog, "agent", "prompt", "pane-1"); operation != nil {
		t.Fatalf("Qoder prompt used brittle agent integration: %v", operation)
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

func TestReadQoderHistoryRespectsRequestedLines(t *testing.T) {
	firstFrame := strings.Repeat("first history row\n", 99) + "first history row"
	secondFrame := strings.Repeat("second history row\n", 99) + "second history row"
	scenario, err := json.Marshal(map[string]any{
		"panes": []map[string]any{{
			"pane_id": "pane-1", "agent": "qodercli", "name": "test",
			"agent_status": "idle", "tab_id": "tab-1",
			"workspace_id": "ws-1", "cwd": "/tmp", "revision": 1,
		}},
		"tabs": []map[string]any{{
			"tab_id": "tab-1", "workspace_id": "ws-1", "label": "main", "number": 1, "cwd": "/tmp",
		}},
		"content": map[string]string{"pane-1": firstFrame},
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

	readPane := func() map[string]any {
		t.Helper()
		read, err := json.Marshal(map[string]any{
			"type": "read_pane", "pane_id": "pane-1", "lines": 100, "format": "ansi",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.Write(ctx, websocket.MessageText, read); err != nil {
			t.Fatal(err)
		}
		return readJSON(t, conn, ctx, 5*time.Second)
	}

	readPane()
	setFakePaneContent(t, env, "pane-1", secondFrame)
	response := readPane()
	content, _ := response["content"].(string)
	if got := len(strings.Split(content, "\n")); got > 100 {
		t.Fatalf("merged qoder history has %d lines, want at most 100", got)
	}
	if !strings.Contains(content, "second history row") {
		t.Fatalf("merged qoder history does not contain the latest frame: %q", content)
	}
	if operation := findFakeOperation(
		t,
		env.operationsLog,
		"pane", "read", "pane-1", "--lines", "100", "--source", "recent",
	); operation == nil {
		t.Fatal("Qoder display read did not request current physical terminal rows")
	}
}

func TestHiddenPromptRouteKeepsTheSecretOutOfTextAndScrollback(t *testing.T) {
	scenario, err := json.Marshal(map[string]any{
		"panes": []map[string]any{{
			"pane_id": "pane-1", "agent": "codex", "name": "test",
			"agent_status": "idle", "tab_id": "tab-1",
			"workspace_id": "ws-1", "cwd": "/tmp", "revision": 1,
		}},
		"tabs": []map[string]any{{
			"tab_id": "tab-1", "workspace_id": "ws-1", "label": "main", "number": 1, "cwd": "/tmp",
		}},
		"content": map[string]string{"pane-1": "make install\n[sudo] password for cv:"},
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

	send := func(command map[string]any) map[string]any {
		t.Helper()
		data, err := json.Marshal(command)
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
			t.Fatal(err)
		}
		return readJSON(t, conn, ctx, 5*time.Second)
	}

	response := send(map[string]any{
		"type": "read_pane", "pane_id": "pane-1", "lines": 100, "format": "ansi",
	})
	if response["no_echo"] != true {
		t.Fatalf("sudo prompt was not recognized: %v", response)
	}
	if response["no_echo_prompt"] != "[sudo] password for cv:" {
		t.Errorf("no_echo_prompt = %v", response["no_echo_prompt"])
	}

	result := send(map[string]any{
		"type": "send_secret", "request_id": "secret-1", "protocol": 2,
		"pane_id": "pane-1", "text": "hunter2",
	})
	if result["ok"] != true {
		t.Fatalf("send_secret result = %v", result)
	}
	// One keystroke per rune with its own Enter: `pane send-text` would be
	// wrapped in bracketed paste and corrupt a value read with echo off.
	argv := findFakeOperation(
		t, env.operationsLog,
		"pane", "send-keys", "pane-1", "h", "u", "n", "t", "e", "r", "2", "Enter",
	)
	if argv == nil {
		t.Fatal("the secret was not typed as individual keystrokes")
	}
	if len(argv) != 11 {
		t.Fatalf("secret keystrokes carried extra arguments: %v", argv)
	}
	if operation := findFakeOperation(t, env.operationsLog, "pane", "send-text", "pane-1"); operation != nil {
		t.Fatalf("the secret was inserted as text: %v", operation)
	}

	// A text-format read must stay on the visible grid: `recent` reaches the
	// pages above it by scrolling the operator's own pane.
	send(map[string]any{
		"type": "read_pane", "pane_id": "pane-1", "lines": 400, "format": "text",
	})
	if operation := findFakeOperation(
		t, env.operationsLog,
		"pane", "read", "pane-1", "--lines", "400", "--source", "visible", "--format", "text",
	); operation == nil {
		t.Fatal("text-format read did not clamp to the visible grid")
	}
	if operation := findFakeOperation(
		t, env.operationsLog, "pane", "read", "pane-1", "--lines", "400", "--source", "recent",
	); operation != nil {
		t.Fatalf("text-format read harvested scrollback: %v", operation)
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

func TestNavigateStructuredQuestionTwice(t *testing.T) {
	firstView := "Question 1/2 (1 unanswered)\nChoose the first value\n\n❯ 1. Alpha\n  2. Beta\n  3. None of the above\n\ntab to add notes | enter to submit answer | ←/→ to navigate questions"
	secondView := "Question 2/2 (1 unanswered)\nChoose the second value\n\n❯ 1. Gamma\n  2. Delta\n  3. None of the above\n\ntab to add notes | enter to submit all | ←/→ to navigate questions"
	scenario, err := json.Marshal(map[string]any{
		"panes": []map[string]any{{
			"pane_id": "pane-1", "agent": "codex", "name": "test",
			"agent_status": "blocked", "tab_id": "tab-1",
			"workspace_id": "ws-1", "cwd": "/tmp", "revision": 1,
		}},
		"tabs": []map[string]any{{
			"tab_id": "tab-1", "workspace_id": "ws-1", "label": "main", "number": 1, "cwd": "/tmp",
		}},
		"content": map[string]string{"pane-1": secondView},
	})
	if err != nil {
		t.Fatal(err)
	}
	env := setupEnvWithScenario(t, string(scenario))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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
	second, ok := content["interaction"].(map[string]any)
	if !ok || second["question_index"] != float64(2) {
		t.Fatalf("second interaction = %+v", content["interaction"])
	}

	for cycle := 1; cycle <= 2; cycle++ {
		requestID := "navigate-" + string(rune('0'+cycle))
		navigate, _ := json.Marshal(map[string]any{
			"type": "navigate_question", "protocol": 2, "request_id": requestID,
			"pane_id": "pane-1", "interaction_id": second["id"], "direction": "previous",
		})
		if err := conn.Write(ctx, websocket.MessageText, navigate); err != nil {
			t.Fatal(err)
		}
		accepted := readJSON(t, conn, ctx, 5*time.Second)
		if accepted["ok"] != true || accepted["phase"] != "accepted" {
			t.Fatalf("cycle %d accepted = %+v", cycle, accepted)
		}
		setFakePaneContent(t, env, "pane-1", firstView)
		terminal := readJSON(t, conn, ctx, 5*time.Second)
		data, _ := terminal["data"].(map[string]any)
		interaction, _ := data["interaction"].(map[string]any)
		if terminal["ok"] != true || terminal["phase"] != "navigated" ||
			interaction["question_index"] != float64(1) {
			t.Fatalf("cycle %d terminal = %+v", cycle, terminal)
		}
		setFakePaneContent(t, env, "pane-1", secondView)
	}
	if got := countFakeOperations(t, env.operationsLog, "pane", "send-keys"); got != 2 {
		t.Fatalf("navigation send-keys operations = %d, want 2", got)
	}
}

func setFakePaneContent(t *testing.T, env *TestEnv, paneID, content string) {
	t.Helper()
	statePath := filepath.Join(env.tmpDir, "scenario.json.state")
	lock, err := os.OpenFile(statePath+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	contents, _ := state["content"].(map[string]any)
	if contents == nil {
		contents = make(map[string]any)
		state["content"] = contents
	}
	contents[paneID] = content
	encoded, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	temporary := statePath + ".test-tmp"
	if err := os.WriteFile(temporary, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temporary, statePath); err != nil {
		t.Fatal(err)
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
		// Skip broadcasts and handshake messages, wait for the response we care
		// about. "blocked" belongs here: an attention transition can reach the
		// socket between a command being sent and its reply, and reading it as
		// the reply fails the test for a reason the command never caused.
		switch msg["type"] {
		case "agents", "workspaces", "agent_update", "activity", "push_config", "activity_history",
			"inventory_status", "blocked":
			continue
		}
		return msg
	}
}
