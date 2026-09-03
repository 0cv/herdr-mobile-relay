package blackbox

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func awaitAgentTarget(t *testing.T, conn *websocket.Conn, ctx context.Context) map[string]any {
	t.Helper()
	for {
		message := readNextJSON(t, conn, ctx)
		if message["type"] != "agents" {
			continue
		}
		agents, _ := message["agents"].([]any)
		if len(agents) == 0 {
			request, _ := json.Marshal(map[string]any{"type": "refresh_agents"})
			if err := conn.Write(ctx, websocket.MessageText, request); err != nil {
				t.Fatal(err)
			}
			continue
		}
		agent, _ := agents[0].(map[string]any)
		target := map[string]any{
			"server_session_id": agent["server_session_id"],
			"pane_id":           agent["pane_id"],
			"terminal_id":       agent["terminal_id"],
			"generation":        agent["generation"],
		}
		if target["server_session_id"] == "" || target["pane_id"] == "" || target["terminal_id"] == "" || target["generation"] == nil {
			t.Fatalf("agent target is incomplete: %v", agent)
		}
		return target
	}
}

func TestOnConnectHandshake(t *testing.T) {
	env := setupEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, env.wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	// First message should be push_config
	msg := readNextJSON(t, conn, ctx)
	if msg["type"] != "push_config" {
		t.Fatalf("first message type = %v, want push_config", msg["type"])
	}
	if msg["protocol"] != float64(3) {
		t.Errorf("protocol = %v", msg["protocol"])
	}
	home, _ := os.UserHomeDir()
	if home == "" || msg["home"] != home {
		t.Errorf("home = %v, want %q", msg["home"], home)
	}
	caps, ok := msg["capabilities"].([]any)
	if !ok || len(caps) == 0 {
		t.Error("capabilities missing or empty")
	}

	// Second message should be agents
	msg = readNextJSON(t, conn, ctx)
	if msg["type"] != "agents" {
		t.Fatalf("second message type = %v, want agents", msg["type"])
	}
}

func TestInstallUpdateBootstrapDoesNotRequireProtocolV2(t *testing.T) {
	env := setupEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, env.wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	payload, err := json.Marshal(map[string]any{
		"type":       "install_update",
		"request_id": "bootstrap-update",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
		t.Fatal(err)
	}
	for {
		message := readNextJSON(t, conn, ctx)
		if message["request_id"] != "bootstrap-update" {
			continue
		}
		if message["type"] != "command_result" || message["action"] != "install_update" {
			t.Fatalf("bootstrap install_update response = %+v", message)
		}
		if message["phase"] == "incompatible_protocol" {
			t.Fatalf("bootstrap install_update required protocol v2: %+v", message)
		}
		return
	}
}

func TestUnknownServerSessionCannotFallThroughToPrimary(t *testing.T) {
	env := setupEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, env.wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	payload, err := json.Marshal(map[string]any{
		"type":              "submit_prompt",
		"protocol":          3,
		"request_id":        "unknown-session",
		"server_session_id": "named-session",
		"pane_id":           "pane-1",
		"text":              "must not reach primary",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
		t.Fatal(err)
	}
	for {
		message := readNextJSON(t, conn, ctx)
		if message["request_id"] != "unknown-session" {
			continue
		}
		apiError, _ := message["error"].(map[string]any)
		if message["type"] != "error" || apiError["code"] != "invalid_request" {
			t.Fatalf("unknown server session response = %+v", message)
		}
		break
	}
	if operation := findFakeOperation(t, env.operationsLog, "agent", "prompt"); operation != nil {
		t.Fatalf("unknown server session reached primary dispatcher: %#v", operation)
	}
}

func TestUploadAttachmentBatch(t *testing.T) {
	env := setupEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, env.wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	png := []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
		0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53,
		0xde, 0x00, 0x00, 0x00, 0x0c, 0x49, 0x44, 0x41,
		0x54, 0x08, 0xd7, 0x63, 0xf8, 0xcf, 0xc0, 0x00,
		0x00, 0x00, 0x02, 0x00, 0x01, 0xe2, 0x21, 0xbc,
		0x33, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e,
		0x44, 0xae, 0x42, 0x60, 0x82,
	}
	target := awaitAgentTarget(t, conn, ctx)
	begin := map[string]any{
		"type":       "upload_begin",
		"protocol":   3,
		"request_id": "upload-begin-1",
		"target":     target,
		"files": []map[string]any{{
			"name": "test-screenshot.png", "media_type": "image/png", "bytes": len(png),
		}},
	}
	data, _ := json.Marshal(begin)
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write begin: %v", err)
	}
	beginMessage := readJSON(t, conn, ctx, 5*time.Second)
	if beginMessage["type"] != "upload_begin_result" {
		t.Fatalf("begin type = %v", beginMessage["type"])
	}
	beginResult, _ := beginMessage["result"].(map[string]any)
	uploadID, _ := beginResult["upload_id"].(string)
	if uploadID == "" {
		t.Fatal("upload id is empty")
	}

	digest := fmt.Sprintf("%x", sha256.Sum256(png))
	chunk := map[string]any{
		"type": "upload_chunk", "protocol": 3, "request_id": "upload-chunk-1",
		"target": target, "upload_id": uploadID, "file_index": 0, "sequence": 0,
		"data": base64.StdEncoding.EncodeToString(png), "sha256": digest,
	}
	data, _ = json.Marshal(chunk)
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write chunk: %v", err)
	}
	chunkMessage := readJSON(t, conn, ctx, 5*time.Second)
	if chunkMessage["type"] != "upload_chunk_result" {
		t.Fatalf("chunk type = %v", chunkMessage["type"])
	}

	finish := map[string]any{
		"type": "upload_finish", "protocol": 3, "request_id": "upload-finish-1",
		"target": target, "upload_id": uploadID,
		"files": []map[string]any{{"file_index": 0, "sha256": digest}},
	}
	data, _ = json.Marshal(finish)
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write finish: %v", err)
	}
	finishMessage := readJSON(t, conn, ctx, 5*time.Second)
	if finishMessage["type"] != "upload_finish_result" {
		t.Fatalf("finish type = %v", finishMessage["type"])
	}
	finishResult, _ := finishMessage["result"].(map[string]any)
	attachments, _ := finishResult["attachments"].([]any)
	if len(attachments) != 1 {
		t.Fatalf("attachments = %v", finishResult["attachments"])
	}
	attachment, _ := attachments[0].(map[string]any)
	ref, _ := attachment["ref"].(string)
	if ref == "" {
		t.Fatal("attachment reference is empty")
	}
	if _, exists := attachment["path"]; exists {
		t.Fatal("attachment leaked its host path")
	}
	prompt := map[string]any{
		"type": "submit_prompt", "protocol": 3, "request_id": "attachment-prompt",
		"pane_id": "pane-1", "target": target, "text": "Review this file:\nAttachment: " + ref,
	}
	data, _ = json.Marshal(prompt)
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	result := readJSON(t, conn, ctx, 5*time.Second)
	if result["type"] != "command_result" || result["ok"] != true {
		t.Fatalf("attachment prompt result = %v", result)
	}
	operation := findFakeOperation(t, env.operationsLog, "agent", "prompt", "pane-1")
	if len(operation) < 4 || !strings.Contains(operation[3], "/uploads/objects/") || operation[3] == "Review this file:\nAttachment: "+ref {
		t.Fatalf("attachment prompt did not resolve an opaque reference: %v", operation)
	}
}

func TestUploadAttachmentRejectsBadMime(t *testing.T) {
	env := setupEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, env.wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	target := awaitAgentTarget(t, conn, ctx)
	request := map[string]any{
		"type": "upload_begin", "protocol": 3, "request_id": "upload-bad",
		"target": target,
		"files": []map[string]any{{
			"name": "evil.exe", "media_type": "application/octet-stream", "bytes": 3,
		}},
	}
	data, _ := json.Marshal(request)
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write: %v", err)
	}
	message := readJSON(t, conn, ctx, 5*time.Second)
	if message["type"] != "upload_begin_result" {
		t.Fatalf("type = %v", message["type"])
	}
	apiError, _ := message["error"].(map[string]any)
	if apiError["code"] != "attachment_unknown_mime" {
		t.Fatalf("error = %v", message["error"])
	}
}

func TestListDirectories(t *testing.T) {
	env := setupEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, env.wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	req := map[string]any{
		"type":       "list_directories",
		"request_id": "ls-1",
		"path":       "",
	}
	data, _ := json.Marshal(req)
	conn.Write(ctx, websocket.MessageText, data)

	msg := readJSON(t, conn, ctx, 5*time.Second)
	if msg["type"] != "command_result" {
		t.Fatalf("type = %v", msg["type"])
	}
	if msg["action"] != "list_directories" {
		t.Errorf("action = %v", msg["action"])
	}
	if msg["ok"] != true {
		t.Errorf("ok = %v", msg["ok"])
	}
	resultData, ok := msg["data"].(map[string]any)
	if !ok {
		t.Fatal("data is not an object")
	}
	current, ok := resultData["current"].(map[string]any)
	if !ok {
		t.Fatal("current is not an object")
	}
	if current["path"] == "" {
		t.Error("current.path is empty")
	}
}

func TestListSlashCommands(t *testing.T) {
	env := setupEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, env.wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	// Wait for the initial agents snapshot so the inventory is committed.
	for {
		waitCtx, waitCancel := context.WithTimeout(ctx, 5*time.Second)
		_, data, err := conn.Read(waitCtx)
		waitCancel()
		if err != nil {
			t.Fatalf("waiting for agents: %v", err)
		}
		var msg map[string]any
		if json.Unmarshal(data, &msg) == nil && msg["type"] == "agents" {
			break
		}
	}

	req := map[string]any{
		"type":       "list_slash_commands",
		"request_id": "slash-1",
		"pane_id":    "pane-1",
	}
	data, _ := json.Marshal(req)
	conn.Write(ctx, websocket.MessageText, data)

	msg := readJSON(t, conn, ctx, 5*time.Second)
	if msg["type"] != "command_result" {
		t.Fatalf("type = %v", msg["type"])
	}
	if msg["action"] != "list_slash_commands" {
		t.Errorf("action = %v", msg["action"])
	}
	if msg["ok"] != true {
		t.Errorf("ok = %v", msg["ok"])
	}
	resultData, ok := msg["data"].(map[string]any)
	if !ok {
		t.Fatal("data is not an object")
	}
	commands, ok := resultData["commands"].([]any)
	if !ok || len(commands) == 0 {
		t.Fatal("commands missing or empty")
	}
	first := commands[0].(map[string]any)
	if first["command"] == "" {
		t.Error("first command has empty name")
	}
}

func TestUDPEventTriggersBroadcast(t *testing.T) {
	env := setupEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, env.wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	// Drain handshake messages
	for i := 0; i < 3; i++ {
		readCtx, readCancel := context.WithTimeout(ctx, 1*time.Second)
		conn.Read(readCtx)
		readCancel()
	}

	// Send a UDP event to the plugin port
	udpAddr := fmt.Sprintf("127.0.0.1:%d", env.pluginPort)
	event := fmt.Sprintf(`{"type":"agent_event","socket_path":%q,"pane_id":"pane-1","status":"blocked","updated_at":"2026-07-22T12:00:00Z"}`, env.socketPath)
	udpConn, err := net.Dial("udp", udpAddr)
	if err != nil {
		t.Fatalf("udp dial: %v", err)
	}
	defer udpConn.Close()
	udpConn.Write([]byte(event))

	// Should receive agent_update broadcast
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("did not receive agent_update after UDP event")
		default:
		}

		readCtx, readCancel := context.WithTimeout(ctx, 2*time.Second)
		_, data, err := conn.Read(readCtx)
		readCancel()
		if err != nil {
			continue
		}

		var msg map[string]any
		if json.Unmarshal(data, &msg) != nil {
			continue
		}

		if msg["type"] == "agent_update" {
			if msg["pane_id"] != "pane-1" {
				t.Errorf("pane_id = %v", msg["pane_id"])
			}
			if msg["status"] != "blocked" {
				t.Errorf("status = %v", msg["status"])
			}
			if msg["event_id"] == "" {
				t.Error("blocked agent_update has no event_id")
			}
			return
		}
	}
}

func TestApprovalRequiresCurrentInventoryEventAndDispatchesOnce(t *testing.T) {
	scenario, err := json.Marshal(map[string]any{
		"panes": []map[string]any{{
			"pane_id": "pane-1", "agent": "claude", "name": "test",
			"agent_status": "blocked", "tab_id": "tab-1",
			"workspace_id": "ws-1", "cwd": "/tmp", "revision": 1,
		}},
		"tabs": []map[string]any{{
			"tab_id": "tab-1", "workspace_id": "ws-1", "label": "main", "number": 1, "cwd": "/tmp",
		}},
		"content": map[string]string{
			"pane-1": "Do you want to proceed?\nBash command\n$ make release\n❯ 1. Approve once\n  2. Always allow\n  3. Reject\nEsc to cancel · Enter to confirm",
		},
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

	var eventID string
	var approvalFingerprint string
	for eventID == "" || approvalFingerprint == "" {
		msg := readNextJSON(t, conn, ctx)
		if msg["type"] != "agents" {
			continue
		}
		agents, _ := msg["agents"].([]any)
		if len(agents) != 1 {
			continue
		}
		agent, _ := agents[0].(map[string]any)
		eventID, _ = agent["event_id"].(string)
		approvalFingerprint, _ = agent["approval_fingerprint"].(string)
		options, _ := agent["options"].([]any)
		if eventID != "" && len(options) != 3 {
			t.Fatalf("approval options = %#v, want 3 choices", agent["options"])
		}
	}

	sendApproval := func(requestID, approvalEventID string) {
		t.Helper()
		payload, marshalErr := json.Marshal(map[string]any{
			"type": "respond", "protocol": 3, "request_id": requestID,
			"pane_id": "pane-1", "event_id": approvalEventID,
			"approval_fingerprint": approvalFingerprint,
			"choice":               "Approve once", "index": 0, "total": 3,
		})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if writeErr := conn.Write(ctx, websocket.MessageText, payload); writeErr != nil {
			t.Fatal(writeErr)
		}
	}

	sendApproval("approval-stale", "stale-event")
	stale := readJSON(t, conn, ctx, 5*time.Second)
	if stale["ok"] != false || stale["error"] != "This approval request is no longer current" {
		t.Fatalf("stale approval result = %+v", stale)
	}
	if got := countFakeOperations(t, env.operationsLog, "pane", "send-keys"); got != 0 {
		t.Fatalf("stale approval dispatched %d send-keys commands", got)
	}

	sendApproval("approval-current", eventID)
	current := readJSON(t, conn, ctx, 5*time.Second)
	if current["ok"] != true || current["phase"] != "accepted" {
		t.Fatalf("current approval result = %+v", current)
	}
	if got := countFakeOperations(t, env.operationsLog, "pane", "send-keys"); got != 1 {
		t.Fatalf("current approval dispatched %d send-keys commands, want 1", got)
	}
}

func TestCapturedQoderAttentionFlowsThroughRelay(t *testing.T) {
	approval := readAttentionCapture(t, "qodercli-permission-required2.ansi")
	notes := readAttentionCapture(t, "qodercli-multi-questions-and-notes.ansi")
	scenario, err := json.Marshal(map[string]any{
		"panes": []map[string]any{
			{
				"pane_id": "approval-pane", "agent": "qodercli", "name": "approval",
				"agent_status": "blocked", "tab_id": "tab-1",
				"workspace_id": "ws-1", "cwd": "/tmp/qoder-approval", "revision": 1,
			},
			{
				"pane_id": "notes-pane", "agent": "qodercli", "name": "notes",
				"agent_status": "blocked", "tab_id": "tab-2",
				"workspace_id": "ws-1", "cwd": "/tmp/qoder-notes", "revision": 1,
			},
		},
		"tabs": []map[string]any{
			{"tab_id": "tab-1", "workspace_id": "ws-1", "label": "approval", "number": 1, "cwd": "/tmp/qoder-approval"},
			{"tab_id": "tab-2", "workspace_id": "ws-1", "label": "notes", "number": 2, "cwd": "/tmp/qoder-notes"},
		},
		"content": map[string]string{
			"approval-pane": approval,
			"notes-pane":    notes,
		},
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

	var approvalEventID string
	var approvalFingerprint string
	var approvalChoice string
	notesSeen := false
	deadline := time.After(8 * time.Second)
	for approvalEventID == "" || approvalFingerprint == "" || !notesSeen {
		select {
		case <-deadline:
			t.Fatalf(
				"captured states not classified: approval event %q, notes %t",
				approvalEventID,
				notesSeen,
			)
		default:
		}
		message := readNextJSON(t, conn, ctx)
		var agents []any
		switch message["type"] {
		case "agents":
			agents, _ = message["agents"].([]any)
		case "blocked":
			agents = []any{message}
		default:
			continue
		}
		for _, raw := range agents {
			agent, _ := raw.(map[string]any)
			switch agent["pane_id"] {
			case "approval-pane":
				options, _ := agent["options"].([]any)
				if agent["attention_kind"] == "approval" && len(options) == 5 {
					approvalEventID, _ = agent["event_id"].(string)
					approvalFingerprint, _ = agent["approval_fingerprint"].(string)
					approvalChoice, _ = options[0].(string)
				}
			case "notes-pane":
				interaction, _ := agent["interaction"].(map[string]any)
				other, _ := interaction["other"].(map[string]any)
				notesSeen = agent["attention_kind"] == "question" &&
					interaction["question"] == "Who's coming along, and how will you get there?" &&
					other["text"] == "I typed some notes here..."
			}
		}
	}

	payload, err := json.Marshal(map[string]any{
		"type":                 "respond",
		"protocol":             3,
		"request_id":           "qoder-approval",
		"pane_id":              "approval-pane",
		"event_id":             approvalEventID,
		"approval_fingerprint": approvalFingerprint,
		"choice":               approvalChoice,
		"index":                0,
		"total":                5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
		t.Fatal(err)
	}
	result := readJSON(t, conn, ctx, 5*time.Second)
	if result["ok"] != true || result["phase"] != "accepted" {
		t.Fatalf("approval result = %+v", result)
	}
	wantKeys := []string{"pane", "send-keys", "approval-pane", "Up", "Up", "Enter"}
	if got := findFakeOperation(t, env.operationsLog, "pane", "send-keys"); !reflect.DeepEqual(got, wantKeys) {
		t.Fatalf("approval operation = %#v, want %#v", got, wantKeys)
	}
}

func readAttentionCapture(t *testing.T, name string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(
		repoRoot(t),
		"internal",
		"question",
		"testdata",
		"attention",
		name,
	))
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func findFakeOperation(t *testing.T, path string, want ...string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var operation struct {
			Argv []string `json:"argv"`
		}
		if err := json.Unmarshal([]byte(line), &operation); err != nil {
			t.Fatalf("decode fake operation: %v", err)
		}
		if len(operation.Argv) < len(want) {
			continue
		}
		match := true
		for index := range want {
			if operation.Argv[index] != want[index] {
				match = false
				break
			}
		}
		if match {
			return operation.Argv
		}
	}
	return nil
}

func countFakeOperations(t *testing.T, path string, want ...string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0
		}
		t.Fatal(err)
	}
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var operation struct {
			Argv []string `json:"argv"`
		}
		if err := json.Unmarshal([]byte(line), &operation); err != nil {
			t.Fatalf("decode fake operation: %v", err)
		}
		if len(operation.Argv) < len(want) {
			continue
		}
		match := true
		for index := range want {
			if operation.Argv[index] != want[index] {
				match = false
				break
			}
		}
		if match {
			count++
		}
	}
	return count
}

func readNextJSON(t *testing.T, conn *websocket.Conn, ctx context.Context) map[string]any {
	t.Helper()
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	_, data, err := conn.Read(readCtx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return msg
}
