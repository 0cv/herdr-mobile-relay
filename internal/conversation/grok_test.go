package conversation

import (
	"context"
	"net/url"
	"path/filepath"
	"testing"
)

const grokCWD = "/work/grok app"

func grokTranscriptPath(home, cwd, sessionID string) string {
	return filepath.Join(home, ".grok", "sessions", url.PathEscape(cwd), sessionID, "chat_history.jsonl")
}

func writeGrokTranscript(t *testing.T, path string) {
	t.Helper()
	writeRows(t, path,
		map[string]any{"type": "system", "content": "You are Grok."},
		map[string]any{"type": "user", "content": []any{map[string]any{"type": "text", "text": "<user_info>\nOS Version: macos\n</user_info>"}}},
		map[string]any{"type": "user", "synthetic_reason": "skills", "content": []any{map[string]any{"type": "text", "text": "<system-reminder>skills</system-reminder>"}}},
		map[string]any{"type": "user", "prompt_index": 0, "content": []any{map[string]any{"type": "text", "text": "<image_files>\n1. /tmp/a.png\n</image_files>\n\n<user_query>\nlist the files\n</user_query>"}}},
		map[string]any{"type": "reasoning", "id": "r1", "summary": []any{map[string]any{"type": "summary_text", "text": "thinking"}}, "encrypted_content": "xyz"},
		map[string]any{"type": "assistant", "content": "", "model_id": "grok", "tool_calls": []any{
			map[string]any{"id": "call-1", "name": "list_dir", "arguments": `{"path":"."}`},
		}},
		map[string]any{"type": "tool_result", "tool_call_id": "call-1", "content": "a.go\nb.go", "images": []any{map[string]any{"type": "image", "url": "data:image/png;base64,AAAA"}}},
		map[string]any{"type": "assistant", "content": "There are two files.", "model_id": "grok"},
		map[string]any{"type": "user", "prompt_index": 1, "synthetic_reason": "task_completed", "content": []any{map[string]any{"type": "text", "text": "<system-reminder>Background task finished</system-reminder>"}}},
		map[string]any{"type": "user", "prompt_index": 2, "content": []any{map[string]any{"type": "text", "text": "Review the change without tags"}}},
	)
}

func assertGrokEntries(t *testing.T, entries []Entry) {
	t.Helper()
	if len(entries) != 4 {
		t.Fatalf("entries = %#v, want 4 visible rows", entries)
	}
	if entries[0].Role != "user" || entries[0].Text != "list the files" {
		t.Fatalf("first entry = %#v, want user query text", entries[0])
	}
	if entries[1].Role != "assistant" || len(entries[1].Tools) != 1 {
		t.Fatalf("tool entry = %#v, want one assistant tool call", entries[1])
	}
	tool := entries[1].Tools[0]
	if tool.Name != "list_dir" || tool.Input != `{"path":"."}` || tool.Output != "a.go\nb.go" {
		t.Fatalf("tool = %#v, want list_dir with its result", tool)
	}
	if entries[2].Role != "assistant" || entries[2].Text != "There are two files." {
		t.Fatalf("assistant entry = %#v", entries[2])
	}
	if entries[3].Role != "user" || entries[3].Text != "Review the change without tags" {
		t.Fatalf("last entry = %#v, want untagged user prompt", entries[3])
	}
}

func TestGrokIsSupported(t *testing.T) {
	if !Supported("grok") || !Supported("Grok") {
		t.Fatal("grok should be a supported conversation provider")
	}
}

func TestGrokConversationReadsChatHistory(t *testing.T) {
	reader, home := testReader(t)
	writeGrokTranscript(t, grokTranscriptPath(home, grokCWD, testSessionID))

	page, err := reader.ReadFor("grok", grokCWD, testSessionID, "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if !page.Available {
		t.Fatalf("page = %#v, want available", page)
	}
	assertGrokEntries(t, page.Entries)
}

func TestGrokLocateFallsBackToSessionScan(t *testing.T) {
	reader, home := testReader(t)
	path := grokTranscriptPath(home, "/somewhere/else", testSessionID)
	writeGrokTranscript(t, path)

	for _, cwd := range []string{"", grokCWD} {
		location := reader.Locate("grok", cwd, testSessionID)
		want, _ := filepath.EvalSymlinks(path)
		if location.Path != want {
			t.Fatalf("Locate(cwd=%q) = %q, want %q", cwd, location.Path, want)
		}
	}
}

func TestGrokLocateRejectsUnsafeSessionIDs(t *testing.T) {
	reader, home := testReader(t)
	writeGrokTranscript(t, grokTranscriptPath(home, grokCWD, testSessionID))

	for _, sessionID := range []string{"", "..", "../" + testSessionID, "a/b"} {
		if location := reader.Locate("grok", grokCWD, sessionID); location.Path != "" {
			t.Fatalf("Locate(%q) = %q, want no location", sessionID, location.Path)
		}
	}
}

func TestGrokBrowserReadsRecentPage(t *testing.T) {
	reader, home := testReader(t)
	writeGrokTranscript(t, grokTranscriptPath(home, grokCWD, testSessionID))
	browser, err := NewBrowser(reader, t.TempDir(), DefaultBrowserOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()

	page, err := browser.ReadPage(context.Background(), BrowseRequest{
		Scope: BrowseScope{Provider: "grok", CWD: grokCWD, SessionID: testSessionID},
		Limit: 10,
	})
	if err != nil || !page.Available {
		t.Fatalf("page = %#v, err = %v", page, err)
	}
	assertGrokEntries(t, page.Entries)
}
