package app

import (
	"strings"
	"testing"
)

func TestAuditWriteDetailsKeepAttributionWithoutPayloadContent(t *testing.T) {
	indices := make([]any, 300)
	for index := range indices {
		indices[index] = float64(index)
	}
	details := auditWriteDetails(map[string]any{
		"type":             "submit_prompt",
		"text":             "private prompt",
		"cwd":              strings.Repeat("界", 1400),
		"keys":             []any{"Ctrl+C", strings.Repeat("k", 100)},
		"selected_indices": indices,
		"index":            float64(2),
		"insert_index":     float64(3),
		"unexpected":       strings.Repeat("secret", 1000),
	})

	if details["text_bytes"] != len("private prompt") {
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
	}
	if cwd, ok := details["cwd"].(string); !ok || len([]rune(cwd)) != 1024 {
		t.Fatalf("bounded cwd = %#v", details["cwd"])
	}
	if keys, ok := details["keys"].([]string); !ok || len(keys) != 2 || len([]rune(keys[1])) != 64 {
		t.Fatalf("keys = %#v", details["keys"])
	}
	if selected, ok := details["selected_indices"].([]int64); !ok || len(selected) != 128 {
		t.Fatalf("selected_indices = %#v", details["selected_indices"])
	}
	if details["insert_index"] != int64(3) {
		t.Fatalf("insert_index = %#v", details["insert_index"])
	}
}

// A secret answering a noecho prompt is short and low entropy: a payload digest
// would be crackable offline, and the per-rune keys spell it out verbatim.
func TestAuditWriteDetailsRedactsSecretInput(t *testing.T) {
	details := auditWriteDetails(map[string]any{
		"type":       "send_secret",
		"request_id": "req-secret",
		"pane_id":    "pane-1",
		"text":       "hunter2",
		"keys":       []any{"h", "u", "n", "t", "e", "r", "2", "Enter"},
	})

	if details["text_bytes"] != len("hunter2") {
		t.Fatalf("text_bytes = %#v", details["text_bytes"])
	}
	for _, key := range []string{"payload_sha256", "payload_bytes", "keys", "text"} {
		if _, exists := details[key]; exists {
			t.Fatalf("secret audit details retained %q: %#v", key, details[key])
		}
	}
	if len(details) != 1 {
		t.Fatalf("secret audit details = %#v, want text_bytes only", details)
	}
}

func TestAuditedWriteSetCoversRemoteAgentMutations(t *testing.T) {
	for _, action := range []string{
		"submit_prompt", "send_keys", "send_text", "respond", "answer_question",
		"navigate_question", "clarify_question", "agent_stop", "agent_rename",
		"tab_reorder", "agent_start", "agent_clear", "agent_restart", "workspace_create",
		"workspace_rename", "workspace_reorder", "workspace_close", "worktree_create",
		"worktree_open", "worktree_remove", "upload_image", "send_secret",
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
