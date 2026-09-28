package conversation

import (
	"path/filepath"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
)

func TestCursorConversationReadsAgentTranscripts(t *testing.T) {
	reader, home := testReader(t)
	t.Setenv(agentroots.CursorListEnv, "")
	cwd := filepath.Join(home, "work", "app")
	slug := cursorProjectSlug(cwd)
	if slug == "" {
		t.Fatal("expected slug for absolute cwd")
	}
	path := filepath.Join(home, ".cursor", "projects", slug, "agent-transcripts", testSessionID, testSessionID+".jsonl")
	writeRows(t, path,
		map[string]any{
			"role": "user",
			"message": map[string]any{
				"content": []any{map[string]any{
					"type": "text",
					"text": "<timestamp>Monday</timestamp>\n<user_query>\nship it\n</user_query>",
				}},
			},
		},
		map[string]any{
			"role": "assistant",
			"message": map[string]any{
				"content": []any{
					map[string]any{"type": "text", "text": "working"},
					map[string]any{"type": "tool_use", "name": "Read", "input": map[string]any{"path": "a.go"}},
				},
			},
		},
	)

	page, err := reader.ReadFor("cursor", cwd, testSessionID, "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if !page.Available || page.Total != 2 || len(page.Entries) != 2 {
		t.Fatalf("page = %#v", page)
	}
	if page.Entries[0].Role != "user" || page.Entries[0].Text != "ship it" {
		t.Fatalf("user entry = %#v", page.Entries[0])
	}
	if page.Entries[1].Role != "assistant" || page.Entries[1].Text != "working" {
		t.Fatalf("assistant entry = %#v", page.Entries[1])
	}
	if len(page.Entries[1].Tools) != 1 || page.Entries[1].Tools[0].Name != "Read" {
		t.Fatalf("tools = %#v", page.Entries[1].Tools)
	}
}

func TestCursorConversationLocatesByUUIDScan(t *testing.T) {
	reader, home := testReader(t)
	t.Setenv(agentroots.CursorListEnv, "")
	path := filepath.Join(home, ".cursor", "projects", "other-slug", "agent-transcripts", testSessionID, testSessionID+".jsonl")
	writeRows(t, path,
		map[string]any{"role": "user", "message": map[string]any{"content": "hello"}},
		map[string]any{"role": "assistant", "message": map[string]any{"content": "hi"}},
	)

	page, err := reader.ReadFor("cursor-agent", filepath.Join(home, "missing", "cwd"), testSessionID, "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if !page.Available || page.Total != 2 || page.Entries[0].Text != "hello" {
		t.Fatalf("page = %#v", page)
	}
}

func TestCursorConversationRejectsNonUUIDSession(t *testing.T) {
	reader, home := testReader(t)
	page, err := reader.ReadFor("cursor", home, "not-a-uuid", "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if page.Available || page.ReasonCode != "invalid_session" {
		t.Fatalf("page = %#v", page)
	}
}

func TestCursorProjectSlug(t *testing.T) {
	got := cursorProjectSlug("/home/vivojf/.herdr/worktrees/pharmacy-ops/products")
	want := "home-vivojf-herdr-worktrees-pharmacy-ops-products"
	if got != want {
		t.Fatalf("slug = %q, want %q", got, want)
	}
}

func TestSupportedIncludesCursor(t *testing.T) {
	for _, name := range []string{"cursor", "Cursor", "cursor-agent", "cursor agent"} {
		if !Supported(name) {
			t.Fatalf("Supported(%q) = false", name)
		}
	}
}
