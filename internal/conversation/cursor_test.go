package conversation

import (
	"context"
	"encoding/json"
	"os"
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
	if page.Entries[0].Role != "user" || page.Entries[0].Text != "ship it" || page.Entries[0].Timestamp != "Monday" {
		t.Fatalf("user entry = %#v", page.Entries[0])
	}
	if page.Entries[1].Role != "assistant" || page.Entries[1].Text != "working" {
		t.Fatalf("assistant entry = %#v", page.Entries[1])
	}
	if len(page.Entries[1].Tools) != 1 || page.Entries[1].Tools[0].Name != "Read" {
		t.Fatalf("tools = %#v", page.Entries[1].Tools)
	}
}

func TestCursorConversationLocatesByWorkspaceTrusted(t *testing.T) {
	reader, home := testReader(t)
	t.Setenv(agentroots.CursorListEnv, "")
	cwd := filepath.Join(home, "real", "workspace")
	projectDir := filepath.Join(home, ".cursor", "projects", "opaque-hash-slug")
	if err := os.MkdirAll(filepath.Join(projectDir, "agent-transcripts", testSessionID), 0o700); err != nil {
		t.Fatal(err)
	}
	trusted, err := json.Marshal(map[string]any{"workspacePath": cwd, "trustedAt": "2026-01-01T00:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, ".workspace-trusted"), trusted, 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(projectDir, "agent-transcripts", testSessionID, testSessionID+".jsonl")
	writeRows(t, path,
		map[string]any{"role": "user", "message": map[string]any{"content": "<user_query>via trust</user_query>"}},
		map[string]any{"role": "assistant", "message": map[string]any{"content": "ok"}},
	)

	page, err := reader.ReadFor("cursor", cwd, testSessionID, "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if !page.Available || page.Total != 2 || page.Entries[0].Text != "via trust" {
		t.Fatalf("page = %#v", page)
	}
}

func TestCursorConversationLocatesByUUIDScan(t *testing.T) {
	reader, home := testReader(t)
	t.Setenv(agentroots.CursorListEnv, "")
	path := filepath.Join(home, ".cursor", "projects", "other-slug", "agent-transcripts", testSessionID, testSessionID+".jsonl")
	writeRows(t, path,
		map[string]any{"role": "user", "message": map[string]any{"content": "<user_query>hello</user_query>"}},
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

func TestCursorConversationLocatesFlatTranscriptLayout(t *testing.T) {
	reader, home := testReader(t)
	t.Setenv(agentroots.CursorListEnv, "")
	cwd := filepath.Join(home, "flat", "proj")
	slug := cursorProjectSlug(cwd)
	path := filepath.Join(home, ".cursor", "projects", slug, "agent-transcripts", testSessionID+".jsonl")
	writeRows(t, path,
		map[string]any{"role": "user", "message": map[string]any{"content": "<user_query>flat</user_query>"}},
		map[string]any{"role": "assistant", "message": map[string]any{"content": "ok"}},
	)
	page, err := reader.ReadFor("cursor", cwd, testSessionID, "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if !page.Available || page.Entries[0].Text != "flat" {
		t.Fatalf("page = %#v", page)
	}
}

func TestCursorConversationHidesHarnessUserText(t *testing.T) {
	reader, home := testReader(t)
	t.Setenv(agentroots.CursorListEnv, "")
	cwd := filepath.Join(home, "work", "app")
	slug := cursorProjectSlug(cwd)
	path := filepath.Join(home, ".cursor", "projects", slug, "agent-transcripts", testSessionID, testSessionID+".jsonl")
	writeRows(t, path,
		map[string]any{"role": "user", "message": map[string]any{"content": "injected harness noise"}},
		map[string]any{"role": "user", "message": map[string]any{"content": "<user_query>real</user_query>"}},
		map[string]any{"role": "assistant", "message": map[string]any{"content": "answer"}},
	)
	page, err := reader.ReadFor("cursor", cwd, testSessionID, "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if !page.Available || page.Total != 2 || page.Entries[0].Text != "real" {
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
	cases := []struct {
		cwd, want string
	}{
		{"/Users/christophe.vidal/Documents", "Users-christophe-vidal-Documents"},
		{"/home/vivojf/.herdr/worktrees/pharmacy-ops/products", "home-vivojf-herdr-worktrees-pharmacy-ops-products"},
		{"/home/vivojf/infrastructure", "home-vivojf-infrastructure"},
	}
	for _, tc := range cases {
		if got := cursorProjectSlug(tc.cwd); got != tc.want {
			t.Fatalf("slug(%q) = %q, want %q", tc.cwd, got, tc.want)
		}
	}
}

func TestSupportedIncludesCursor(t *testing.T) {
	for _, name := range []string{"cursor", "Cursor", "cursor-agent", "cursor agent"} {
		if !Supported(name) {
			t.Fatalf("Supported(%q) = false", name)
		}
	}
}

func TestBrowserReadPageIncludesCursorTextTurns(t *testing.T) {
	reader, home := testReader(t)
	t.Setenv(agentroots.CursorListEnv, "")
	cwd := filepath.Join(home, "browse", "app")
	slug := cursorProjectSlug(cwd)
	path := filepath.Join(home, ".cursor", "projects", slug, "agent-transcripts", testSessionID, testSessionID+".jsonl")
	writeRows(t, path,
		map[string]any{"role": "user", "message": map[string]any{"content": "<user_query>one</user_query>"}},
		map[string]any{"role": "assistant", "message": map[string]any{"content": "two"}},
		map[string]any{"role": "assistant", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_use", "name": "Shell", "input": map[string]any{"command": "pwd"}},
		}}},
	)
	browser, err := NewBrowser(reader, t.TempDir(), DefaultBrowserOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "cursor", CWD: cwd, SessionID: testSessionID}
	page, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !page.Available || len(page.Entries) != 3 {
		t.Fatalf("page = %#v", page)
	}
	if page.Entries[0].Role != "user" || page.Entries[0].Text != "one" {
		t.Fatalf("entry0 = %#v", page.Entries[0])
	}
	if page.Entries[1].Role != "assistant" || page.Entries[1].Text != "two" {
		t.Fatalf("entry1 = %#v", page.Entries[1])
	}
	if page.Entries[2].Role != "assistant" || page.Entries[2].Text != "" || len(page.Entries[2].Tools) != 1 {
		t.Fatalf("entry2 = %#v", page.Entries[2])
	}
}
