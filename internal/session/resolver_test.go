package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestQoderSessionName(t *testing.T) {
	home := t.TempDir()
	projDir := filepath.Join(home, ".qoder", "projects", "home-user-myapp")
	os.MkdirAll(projDir, 0o755)

	// Write a session JSONL with a title
	entries := []map[string]any{
		{"type": "summary", "title": "Fix login bug"},
		{"type": "ai-title", "aiTitle": "Authentication fix"},
		{"type": "custom-title", "customTitle": "My Custom Title"},
	}
	var lines []byte
	for _, e := range entries {
		data, _ := json.Marshal(e)
		lines = append(lines, data...)
		lines = append(lines, '\n')
	}
	os.WriteFile(filepath.Join(projDir, "sess-123.jsonl"), lines, 0o644)

	r := NewResolver(home)
	name := r.SessionName("qoder", "/home/user/myapp", "sess-123")
	if name != "My Custom Title" {
		t.Errorf("session name = %q, want 'My Custom Title'", name)
	}
}

func TestQoderLeadingDashSessionName(t *testing.T) {
	home := t.TempDir()
	projDir := filepath.Join(home, ".qoder", "projects", "-home-user-myapp")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	entry := map[string]any{
		"type":        "custom-title",
		"sessionId":   "sess-renamed",
		"customTitle": "Renamed Qoder session",
	}
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, "sess-renamed.jsonl"), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := NewResolver(home).SessionName("qodercli", "/home/user/myapp", "sess-renamed"); got != "Renamed Qoder session" {
		t.Fatalf("session name = %q, want %q", got, "Renamed Qoder session")
	}
}

func TestClaudeSessionName(t *testing.T) {
	home := t.TempDir()
	projDir := filepath.Join(home, ".claude", "projects", "home-user-app")
	os.MkdirAll(projDir, 0o755)

	entry := map[string]any{"type": "summary", "summary": "Refactor database layer"}
	data, _ := json.Marshal(entry)
	os.WriteFile(filepath.Join(projDir, "abc.jsonl"), append(data, '\n'), 0o644)

	r := NewResolver(home)
	name := r.SessionName("claude-code", "/home/user/app", "abc")
	if name != "Refactor database layer" {
		t.Errorf("session name = %q, want 'Refactor database layer'", name)
	}
}

func TestCodexSessionName(t *testing.T) {
	home := t.TempDir()
	codexDir := filepath.Join(home, ".codex")
	os.MkdirAll(codexDir, 0o755)

	entry := map[string]any{"id": "sess-456", "thread_name": "Build API endpoint"}
	data, _ := json.Marshal(entry)
	os.WriteFile(filepath.Join(codexDir, "session_index.jsonl"), append(data, '\n'), 0o644)

	r := NewResolver(home)
	name := r.SessionName("codex", "/tmp", "sess-456")
	if name != "Build API endpoint" {
		t.Errorf("session name = %q, want 'Build API endpoint'", name)
	}
}

func TestEmptySessionID(t *testing.T) {
	home := t.TempDir()
	r := NewResolver(home)
	name := r.SessionName("claude", "/tmp", "")
	if name != "" {
		t.Errorf("empty session ID should return empty, got %q", name)
	}
}

func TestUnknownAgent(t *testing.T) {
	home := t.TempDir()
	r := NewResolver(home)
	name := r.SessionName("unknown-agent", "/tmp", "sess-1")
	if name != "" {
		t.Errorf("unknown agent should return empty, got %q", name)
	}
}

func TestCacheTTL(t *testing.T) {
	home := t.TempDir()
	projDir := filepath.Join(home, ".qoder", "projects", "home-user-proj")
	os.MkdirAll(projDir, 0o755)

	entry := map[string]any{"type": "summary", "title": "First Title"}
	data, _ := json.Marshal(entry)
	os.WriteFile(filepath.Join(projDir, "s1.jsonl"), append(data, '\n'), 0o644)

	r := NewResolver(home)
	name1 := r.SessionName("qoder", "/home/user/proj", "s1")
	if name1 != "First Title" {
		t.Fatalf("first call = %q", name1)
	}

	// Overwrite file — the source signature invalidates the cache immediately.
	entry2 := map[string]any{"type": "summary", "title": "Second Title"}
	data2, _ := json.Marshal(entry2)
	os.WriteFile(filepath.Join(projDir, "s1.jsonl"), append(data2, '\n'), 0o644)

	name2 := r.SessionName("qoder", "/home/user/proj", "s1")
	if name2 != "Second Title" {
		t.Errorf("refreshed call = %q, want 'Second Title'", name2)
	}
}

func TestExactSessionIDDoesNotFallBackToOnlyOtherFile(t *testing.T) {
	home := t.TempDir()
	projDir := filepath.Join(home, ".claude", "projects", "home-user-app")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, "other.jsonl"), []byte("{\"type\":\"custom-title\",\"customTitle\":\"Wrong\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := NewResolver(home).SessionName("claude", "/home/user/app", "wanted"); got != "" {
		t.Fatalf("session name = %q, want empty exact-ID miss", got)
	}
}
