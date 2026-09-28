package conversation

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func (f *grokFixture) untaggedUser(text string) {
	f.add(map[string]any{
		"sessionUpdate": "user_message_chunk",
		"content":       map[string]any{"type": "text", "text": text},
	})
}

func (f *grokFixture) rewind(target any) {
	update := map[string]any{"sessionUpdate": "rewind_marker", "created_at": "2027-01-15T08:00:00Z"}
	if target != nil {
		update["target_prompt_index"] = target
	}
	f.add(update)
}

func (f *grokFixture) rawToolResult(id string, rawOutput map[string]any) {
	f.add(map[string]any{
		"sessionUpdate": "tool_call_update", "toolCallId": id, "status": "completed", "rawOutput": rawOutput,
	})
}

func grokEntryTexts(entries []Entry) []string {
	texts := make([]string, 0, len(entries))
	for _, entry := range entries {
		texts = append(texts, entry.Role+":"+entry.Text)
	}
	return texts
}

func assertGrokTexts(t *testing.T, entries []Entry, want ...string) {
	t.Helper()
	if got := grokEntryTexts(entries); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("entries = %q, want %q", got, want)
	}
}

// rewind_updates.jsonl is a Grok CLI 1.0.41 session reduced to its structural
// records: prompts ONE, TWO and THREE, a TUI /rewind of THREE, then prompt
// FOUR, which Grok gives THREE's promptIndex.
func TestGrokRewindFixtureFromGrokCLI(t *testing.T) {
	reader, home := testReader(t)
	data, err := os.ReadFile(filepath.Join("testdata", "grok", "rewind_updates.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	path := grokUpdatesPath(home, grokCWD, testSessionID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	page := readGrok(t, reader, "", 80)
	prompt := func(word string) string {
		return "user:Reply with exactly the word " + word + " and nothing else. Do not use tools."
	}
	assertGrokTexts(t, page.Entries, prompt("ONE"), "assistant:ONE", prompt("TWO"), "assistant:TWO", prompt("FOUR"), "assistant:FOUR")
	if page.SourceCorrupt {
		t.Fatal("a mapped rewind marker is not corruption")
	}
}

func TestGrokRewindCountsTurnsByPosition(t *testing.T) {
	reader, home := testReader(t)
	f := &grokFixture{}
	f.hostTurn("/always-approve")
	f.other("turn_completed")
	f.user(0, "first")
	f.agent("first reply")
	f.toolCall("call-1", "read_file", map[string]any{"path": "a.go"})
	f.hiddenUser(1, "<system-reminder>background task finished</system-reminder>")
	f.agent("hidden reply")
	f.user(2, "second")
	f.agent("second reply")
	f.rewind(1)
	f.toolResult("call-1", "completed", "late result")
	f.user(1, "replacement")
	f.agent("replacement reply")
	f.write(t, grokUpdatesPath(home, grokCWD, testSessionID))

	page := readGrok(t, reader, "", 80)
	assertGrokTexts(t, page.Entries, "user:/always-approve", "user:first", "assistant:first reply", "user:replacement", "assistant:replacement reply")
	if page.Entries[2].Tools[0].Output != "late result" || page.SourceCorrupt {
		t.Fatalf("page = %#v, want the kept tool call to receive its result", page)
	}
}

func TestGrokRewindUntaggedTurnsAndRepeatedRewinds(t *testing.T) {
	reader, home := testReader(t)
	f := &grokFixture{}
	f.untaggedUser("first")
	f.untaggedUser("first continued")
	f.agent("first reply")
	f.untaggedUser("second")
	f.agent("second reply")
	f.untaggedUser("third")
	f.agent("third reply")
	f.rewind(2)
	f.untaggedUser("fourth")
	f.agent("fourth reply")
	f.rewind(1)
	f.untaggedUser("fifth")
	f.write(t, grokUpdatesPath(home, grokCWD, testSessionID))

	page := readGrok(t, reader, "", 80)
	assertGrokTexts(t, page.Entries, "user:first\nfirst continued", "assistant:first reply", "user:fifth")
}

func TestGrokRewindWithUnmappedTargetIsIgnored(t *testing.T) {
	for _, target := range []any{nil, -1, 3, "1"} {
		reader, home := testReader(t)
		f := grokTurns(2)
		f.rewind(target)
		f.rewind(2)
		f.write(t, grokUpdatesPath(home, grokCWD, testSessionID))

		page := readGrok(t, reader, "", 80)
		assertGrokTexts(t, page.Entries, "user:question 0", "assistant:answer 0", "user:question 1", "assistant:answer 1")
		if !page.SourceCorrupt {
			t.Fatalf("target %v: want the unmapped marker reported as corrupt", target)
		}
	}
}

func TestGrokBrowserRewindChangesRevision(t *testing.T) {
	reader, home := testReader(t)
	path := grokUpdatesPath(home, grokCWD, testSessionID)
	f := grokTurns(3)
	f.write(t, path)
	browser, err := NewBrowser(reader, t.TempDir(), DefaultBrowserOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "grok", CWD: grokCWD, SessionID: testSessionID}

	latest, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 2})
	if err != nil || latest.NextCursor == "" {
		t.Fatalf("latest = %#v, err = %v", latest, err)
	}
	appendGrokRows(t, path, func(f *grokFixture) { f.rewind(1) })
	stale, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: latest.NextCursor, Limit: 2})
	if err != nil || stale.ReasonCode != "source_changed" {
		t.Fatalf("stale = %#v, err = %v, want source_changed after a rewind", stale, err)
	}
	fresh, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 80})
	if err != nil || fresh.SourceRevision == latest.SourceRevision {
		t.Fatalf("fresh = %#v, err = %v, want a new revision", fresh, err)
	}
	assertGrokTexts(t, fresh.Entries, "user:question 0", "assistant:answer 0")
}

func appendGrokRows(t *testing.T, path string, build func(*grokFixture)) {
	t.Helper()
	f := &grokFixture{clock: 100}
	build(f)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	extra := filepath.Join(t.TempDir(), "extra.jsonl")
	f.write(t, extra)
	data, err := os.ReadFile(extra)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(data); err != nil {
		t.Fatal(err)
	}
}

func TestGrokOversizedRecordIsReportedSeparately(t *testing.T) {
	reader, home := testReader(t)
	path := grokUpdatesPath(home, grokCWD, testSessionID)
	grokTurns(1).write(t, path)
	appendGrokRows(t, path, func(f *grokFixture) {
		f.agent(strings.Repeat("x", maxGrokRecordBytes))
		f.user(1, "after the large record")
	})

	page := readGrok(t, reader, "", 80)
	assertGrokTexts(t, page.Entries, "user:question 0", "assistant:answer 0", "user:after the large record")
	if page.SourceCorrupt {
		t.Fatal("an oversized record is not corruption")
	}
	browser, err := NewBrowser(reader, t.TempDir(), DefaultBrowserOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "grok", CWD: grokCWD, SessionID: testSessionID}
	browsed, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 80})
	if err != nil || browsed.Diagnostics.OversizedRecords != 1 || browsed.Diagnostics.CorruptRecords != 0 {
		t.Fatalf("diagnostics = %#v, err = %v, want one oversized record", browsed.Diagnostics, err)
	}
}

func TestGrokRawOutputText(t *testing.T) {
	reader, home := testReader(t)
	f := &grokFixture{}
	f.user(0, "inspect")
	outputs := []map[string]any{
		{"type": "ListDir", "Content": map[string]any{"content": "a.go\nb.go", "absolute_root_path": "/work"}},
		{"type": "TaskOutput", "Result": map[string]any{"output": "task done", "exit_code": 0}},
		{"type": "Text", "text": "plain text"},
		{"type": "Result", "Result": "result text"},
		{"type": "EditsApplied", "EditsApplied": 1},
	}
	for index, output := range outputs {
		id := "call-" + string(rune('a'+index))
		f.toolCall(id, "tool", map[string]any{})
		f.rawToolResult(id, output)
	}
	f.write(t, grokUpdatesPath(home, grokCWD, testSessionID))

	page := readGrok(t, reader, "", 80)
	want := []string{"a.go\nb.go", "task done", "plain text", "result text", `{"EditsApplied":1,"type":"EditsApplied"}`}
	tools := page.Entries[1].Tools
	for index := range want {
		if tools[index].Output != want[index] {
			t.Fatalf("tool %d output = %q, want %q", index, tools[index].Output, want[index])
		}
	}
}
