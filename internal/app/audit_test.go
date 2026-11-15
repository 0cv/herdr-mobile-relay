package app

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/audit"
)

// auditAttempts returns the records written before a write was dispatched.
func (e *frameEnv) auditAttempts(t *testing.T) []audit.Record {
	t.Helper()
	var attempts []audit.Record
	for _, record := range readAuditRecords(t, e.cacheDir) {
		if record.Stage == "attempt" {
			attempts = append(attempts, record)
		}
	}
	return attempts
}

func (e *frameEnv) auditAttempt(t *testing.T, action string) audit.Record {
	t.Helper()
	attempts := e.auditAttempts(t)
	if len(attempts) != 1 || attempts[0].Action != action {
		t.Fatalf("audit attempts = %#v, want one for %s", attempts, action)
	}
	return attempts[0]
}

func TestAuditAttemptKeepsAttributionWithoutPayloadContent(t *testing.T) {
	e := newFrameEnv(t)
	indices := make([]int, 300)
	for index := range indices {
		indices[index] = index
	}
	e.exchange(t, map[string]any{
		"type":             "submit_prompt",
		"request_id":       "audit-1",
		"text":             "private prompt",
		"cwd":              strings.Repeat("界", 1400),
		"keys":             []string{"Ctrl+C", strings.Repeat("k", 100)},
		"selected_indices": indices,
		"index":            2,
		"insert_index":     3,
		"unexpected":       strings.Repeat("secret", 1000),
	})

	record := e.auditAttempt(t, "submit_prompt")
	if record.RequestID != "audit-1" || record.PaneID != "pane-1" {
		t.Fatalf("attribution = %#v", record)
	}
	details := record.Details
	if details["text_bytes"] != float64(len("private prompt")) {
		t.Fatalf("text_bytes = %#v", details["text_bytes"])
	}
	if _, exists := details["text"]; exists {
		t.Fatal("audit details retained prompt content")
	}
	if _, exists := details["unexpected"]; exists {
		t.Fatal("audit details retained an unapproved field")
	}
	if hash, ok := details["payload_sha256"].(string); !ok || len(hash) != 64 {
		t.Fatalf("payload hash = %#v", details["payload_sha256"])
	} else if _, err := hex.DecodeString(hash); err != nil {
		t.Fatalf("payload hash is not hexadecimal: %q", hash)
	}
	if cwd, ok := details["cwd"].(string); !ok || len([]rune(cwd)) != 1024 {
		t.Fatalf("bounded cwd = %#v", details["cwd"])
	}
	if keys, ok := details["keys"].([]any); !ok || len(keys) != 2 || len([]rune(keys[1].(string))) != 64 {
		t.Fatalf("keys = %#v", details["keys"])
	}
	if selected, ok := details["selected_indices"].([]any); !ok || len(selected) != 128 {
		t.Fatalf("selected_indices = %#v", details["selected_indices"])
	}
	if details["index"] != float64(2) || details["insert_index"] != float64(3) {
		t.Fatalf("index = %#v, insert_index = %#v", details["index"], details["insert_index"])
	}
}

func TestAuditDigestIsTheHashOfTheReceivedFrame(t *testing.T) {
	e := newFrameEnv(t)
	frame := []byte(`{ "request_id" : "digest-1", "type":"send_keys",  "protocol":3, "pane_id":"pane-1", "keys":["a"] }`)
	sendRawFrame(t, e.conn, frame)
	expectCommandResult(t, readFrame(t, e.conn), "send_keys", true, "completed", "")

	details := e.auditAttempt(t, "send_keys").Details
	sum := sha256.Sum256(frame)
	if details["payload_sha256"] != hex.EncodeToString(sum[:]) || details["payload_bytes"] != float64(len(frame)) {
		t.Fatalf("digest = %v over %v bytes, want %x over %d bytes", details["payload_sha256"], details["payload_bytes"], sum, len(frame))
	}
}

func TestAuditCountsPromptTextBeforeAttachmentExpansion(t *testing.T) {
	e := newFrameEnv(t)
	target := e.target(t, "pane-1")
	ref := e.uploadAttachment(t, target)
	text := "Review this file:\nAttachment: " + ref

	expectCommandResult(t, e.exchange(t, map[string]any{
		"type": "submit_prompt", "target": target, "text": text,
	}), "submit_prompt", true, "completed", "")

	if prompts := e.backend.called("Prompt"); len(prompts) != 1 || strings.Contains(prompts[0], "Attachment: "+ref) {
		t.Fatalf("prompts = %v, want the reference expanded", prompts)
	}
	attempts := e.auditAttempts(t)
	last := attempts[len(attempts)-1]
	if last.Action != "submit_prompt" || last.Details["text_bytes"] != float64(len(text)) {
		t.Fatalf("last attempt = %#v, want submit_prompt with text_bytes %d", last, len(text))
	}
	for _, record := range readAuditRecords(t, e.cacheDir) {
		if record.Stage == "result" && record.Details != nil {
			t.Fatalf("result record kept details: %#v", record.Details)
		}
	}
}

func TestAuditAttemptCapsKeysAtThirtyTwo(t *testing.T) {
	e := newFrameEnv(t)
	keys := make([]string, 40)
	for index := range keys {
		keys[index] = fmt.Sprintf("k%d", index)
	}
	e.exchange(t, map[string]any{"type": "send_keys", "keys": keys})

	recorded, _ := e.auditAttempt(t, "send_keys").Details["keys"].([]any)
	if len(recorded) != 32 || recorded[0] != "k0" || recorded[31] != "k31" {
		t.Fatalf("keys = %#v, want the first 32", recorded)
	}
}

// A secret answering a noecho prompt is short and low entropy: a payload digest
// would be crackable offline, and the per-rune keys spell it out verbatim.
func TestAuditAttemptRedactsSecretInput(t *testing.T) {
	e := newFrameEnv(t)
	for _, action := range []string{"send_secret", "send_terminal_text"} {
		e.exchange(t, map[string]any{
			"type":       action,
			"request_id": "req-" + action,
			"text":       "hunter2",
			"keys":       []string{"h", "u", "n", "t", "e", "r", "2", "Enter"},
		})
	}

	attempts := e.auditAttempts(t)
	if len(attempts) != 2 {
		t.Fatalf("audit attempts = %#v, want one per secret frame", attempts)
	}
	for _, record := range attempts {
		if record.Details["text_bytes"] != float64(len("hunter2")) {
			t.Fatalf("%s text_bytes = %#v", record.Action, record.Details["text_bytes"])
		}
		for _, key := range []string{"payload_sha256", "payload_bytes", "keys", "text"} {
			if _, exists := record.Details[key]; exists {
				t.Fatalf("%s audit details retained %q: %#v", record.Action, key, record.Details[key])
			}
		}
		if len(record.Details) != 1 {
			t.Fatalf("%s audit details = %#v, want text_bytes only", record.Action, record.Details)
		}
	}
}

func TestAuditAttemptBoundsWorkspaceIDs(t *testing.T) {
	e := newFrameEnv(t)
	ids := []string{"", ""}
	for range 40 {
		ids = append(ids, strings.Repeat("界", 200))
	}
	e.exchange(t, map[string]any{"type": "workspace_reorder", "workspace_ids": ids, "expected_workspace_ids": ids})

	details := e.auditAttempt(t, "workspace_reorder").Details
	for _, key := range []string{"workspace_ids", "expected_workspace_ids"} {
		values, ok := details[key].([]any)
		if !ok || len(values) != 32 {
			t.Fatalf("%s = %#v", key, details[key])
		}
		for _, value := range values {
			if len([]rune(value.(string))) != 160 {
				t.Fatalf("%s entry length = %d, want 160", key, len([]rune(value.(string))))
			}
		}
	}
}

func TestAuditAttemptSkipsEmptyWorkspaceIDsAndRefusesOtherTypes(t *testing.T) {
	e := newFrameEnv(t)
	e.exchange(t, map[string]any{"type": "workspace_reorder", "request_id": "empty", "workspace_ids": []any{nil, ""}})
	if _, exists := e.auditAttempt(t, "workspace_reorder").Details["workspace_ids"]; exists {
		t.Fatal("audit details kept empty workspace ids")
	}

	refused := e.exchange(t, map[string]any{"type": "workspace_reorder", "request_id": "number", "workspace_ids": []any{1}})
	if refused["type"] != "error" {
		t.Fatalf("response = %#v, want a workspace id that is not a string refused", refused)
	}
	if attempts := e.auditAttempts(t); len(attempts) != 1 {
		t.Fatalf("audit attempts = %#v, want none for the refused frame", attempts)
	}
}

func TestAuditIgnoresTheActionKey(t *testing.T) {
	e := newFrameEnv(t)
	var audited []string
	for index, test := range []struct {
		typ, action string
		write       bool
	}{
		{"send_secret", "", true},
		{"send_secret", "refresh_agents", true},
		{"command", "send_secret", false},
		{"", "send_secret", false},
		{"refresh_agents", "send_secret", false},
	} {
		requestID := fmt.Sprintf("req-%d", index)
		if test.write {
			audited = append(audited, requestID)
		}
		sendFrame(t, e.conn, frameWithDefaults(map[string]any{
			"type": test.typ, "action": test.action, "request_id": requestID,
			"text": "fixture", "keys": []string{"f", "Enter"},
		}))
		e.settle(t)
	}

	attempts := e.auditAttempts(t)
	var recorded []string
	for _, record := range attempts {
		recorded = append(recorded, record.RequestID)
		if record.Action != "send_secret" {
			t.Fatalf("%s was audited as %q", record.RequestID, record.Action)
		}
		for _, key := range []string{"payload_sha256", "payload_bytes", "keys", "text"} {
			if _, exists := record.Details[key]; exists {
				t.Fatalf("%s retained %s", record.RequestID, key)
			}
		}
		if record.Details["text_bytes"] != float64(len("fixture")) {
			t.Fatalf("secret shape missing: %#v", record.Details)
		}
	}
	if !slices.Equal(recorded, audited) {
		t.Fatalf("audited requests = %v, want %v", recorded, audited)
	}
}

func TestAuditedWriteSetCoversRemoteAgentMutations(t *testing.T) {
	for _, action := range []string{
		"submit_prompt", "send_keys", "send_input", "respond", "answer_question",
		"navigate_question", "clarify_question", "agent_stop", "agent_rename",
		"tab_reorder", "agent_start", "agent_clear", "workspace_create",
		"workspace_rename", "workspace_reorder", "workspace_close", "worktree_create",
		"worktree_open", "worktree_remove", "upload_begin", "upload_chunk",
		"upload_finish", "upload_cancel", "send_secret", "send_terminal_text",
	} {
		if !isAuditedWrite(action) {
			t.Fatalf("%s is not audited", action)
		}
	}
	for _, action := range []string{"workspace_file", "worktree_list", "get_activity", "refresh_agents"} {
		if isAuditedWrite(action) {
			t.Fatalf("read-only action %s is marked as an audited write", action)
		}
	}
}
