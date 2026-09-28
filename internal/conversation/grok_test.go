package conversation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const grokCWD = "/work/grok app"

// grokFixture builds updates.jsonl rows with the record shape Grok CLI writes
// (method/params/update/_meta). All text in these fixtures is synthetic.
type grokFixture struct {
	rows  []map[string]any
	clock int64
}

func (f *grokFixture) add(update map[string]any) {
	f.clock++
	method := "session/update"
	if len(f.rows)%2 == 1 {
		method = "_x.ai/session/update"
	}
	f.rows = append(f.rows, map[string]any{
		"timestamp": 1_800_000_000 + f.clock,
		"method":    method,
		"params": map[string]any{
			"sessionId": testSessionID,
			"update":    update,
			"_meta": map[string]any{
				"eventId":          fmt.Sprintf("event-%d", f.clock),
				"agentTimestampMs": (1_800_000_000+f.clock)*1000 + 250,
				"promptId":         "prompt-synthetic",
			},
		},
	})
}

func (f *grokFixture) user(index int, text string) {
	f.add(map[string]any{
		"sessionUpdate": "user_message_chunk",
		"content":       map[string]any{"type": "text", "text": text},
		"_meta":         map[string]any{"modelId": "grok-test", "promptIndex": index},
	})
}

func (f *grokFixture) hiddenUser(index int, text string) {
	f.add(map[string]any{
		"sessionUpdate": "user_message_chunk",
		"content":       map[string]any{"type": "text", "text": text},
		"_meta":         map[string]any{"modelId": "grok-test", "promptIndex": index, "hideFromScrollback": true},
	})
}

func (f *grokFixture) hostTurn(text string) {
	f.add(map[string]any{
		"sessionUpdate": "user_message_chunk",
		"content":       map[string]any{"type": "text", "text": text},
		"_meta":         map[string]any{"hostTurn": true},
	})
}

func (f *grokFixture) userImage(index int) {
	f.add(map[string]any{
		"sessionUpdate": "user_message_chunk",
		"content":       map[string]any{"type": "image", "data": "AAAA", "mimeType": "image/png"},
		"_meta":         map[string]any{"modelId": "grok-test", "promptIndex": index},
	})
}

func (f *grokFixture) agent(text string) {
	f.add(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": text}})
}

func (f *grokFixture) thought(text string) {
	f.add(map[string]any{"sessionUpdate": "agent_thought_chunk", "content": map[string]any{"type": "text", "text": text}})
}

func (f *grokFixture) other(kind string) {
	f.add(map[string]any{"sessionUpdate": kind, "detail": map[string]any{"note": "synthetic"}})
}

func (f *grokFixture) hook() {
	f.add(map[string]any{
		"sessionUpdate": "hook_execution",
		"output":        `{"sessionUpdate":"tool_call","toolCallId":"from-hook-output"}`,
	})
}

func grokToolMeta(name string) map[string]any {
	return map[string]any{"x.ai/tool": map[string]any{"name": name, "kind": "read", "label": name, "namespace": "builtin"}}
}

func (f *grokFixture) toolCall(id, name string, input map[string]any) {
	f.add(map[string]any{
		"sessionUpdate": "tool_call", "toolCallId": id, "title": name, "rawInput": input, "_meta": grokToolMeta(name),
	})
}

func (f *grokFixture) toolProgress(id, name string, input map[string]any) {
	f.add(map[string]any{
		"sessionUpdate": "tool_call_update", "toolCallId": id, "title": name, "rawInput": input,
		"kind": "read", "locations": []any{}, "_meta": grokToolMeta(name),
	})
}

func (f *grokFixture) toolResult(id, status, text string) {
	f.add(map[string]any{
		"sessionUpdate": "tool_call_update", "toolCallId": id, "status": status,
		"rawOutput": map[string]any{"type": "Result", "Result": text},
		"content": []any{
			map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": text}},
			map[string]any{"type": "content", "content": map[string]any{"type": "image", "data": "AAAA", "mimeType": "image/png"}},
		},
	})
}

func (f *grokFixture) toolDiff(id string) {
	f.add(map[string]any{
		"sessionUpdate": "tool_call_update", "toolCallId": id, "status": "completed",
		"rawOutput": map[string]any{"type": "EditsApplied", "EditsApplied": 1},
		"content":   []any{map[string]any{"type": "diff", "path": "/work/a.go", "oldText": "a", "newText": "b"}},
	})
}

func grokUpdatesPath(home, cwd, sessionID string) string {
	return filepath.Join(home, ".grok", "sessions", grokProjectDirName(cwd), sessionID, "updates.jsonl")
}

func (f *grokFixture) write(t *testing.T, path string) {
	t.Helper()
	writeRows(t, path, f.rows...)
}

func readGrok(t *testing.T, reader *Reader, before string, limit int) Page {
	t.Helper()
	page, err := reader.ReadFor("grok", grokCWD, testSessionID, before, limit)
	if err != nil {
		t.Fatal(err)
	}
	return page
}

func assertEntry(t *testing.T, entry Entry, role, text string, tools int) {
	t.Helper()
	if entry.Role != role || entry.Text != text || len(entry.Tools) != tools {
		t.Fatalf("entry = %#v, want %s %q with %d tools", entry, role, text, tools)
	}
}

func normalGrokSession() *grokFixture {
	f := &grokFixture{}
	f.other("current_mode_update")
	f.user(0, "list the files")
	f.hook()
	f.thought("thinking about the request")
	f.agent("I'll ")
	f.agent("check.")
	f.toolCall("call-1", "list_dir", map[string]any{"path": "."})
	f.toolProgress("call-1", "list_dir", map[string]any{"path": "."})
	f.hook()
	f.toolResult("call-1", "completed", "a.go\nb.go")
	f.thought("more thinking")
	f.agent("There are ")
	f.agent("two files.")
	f.other("turn_completed")
	return f
}

func TestGrokIsSupported(t *testing.T) {
	if !Supported("grok") || !Supported("Grok") {
		t.Fatal("grok should be a supported conversation provider")
	}
}

func TestGrokReadsNormalSession(t *testing.T) {
	reader, home := testReader(t)
	normalGrokSession().write(t, grokUpdatesPath(home, grokCWD, testSessionID))

	page := readGrok(t, reader, "", 80)
	if !page.Available || page.Total != 3 || page.HasMore || page.SourceCorrupt {
		t.Fatalf("page = %#v, want 3 available entries", page)
	}
	assertEntry(t, page.Entries[0], "user", "list the files", 0)
	assertEntry(t, page.Entries[1], "assistant", "I'll check.", 1)
	assertEntry(t, page.Entries[2], "assistant", "There are two files.", 0)
	tool := page.Entries[1].Tools[0]
	if tool.ID != "call-1" || tool.Name != "list_dir" || tool.Input != `{"path":"."}` || tool.Output != "a.go\nb.go" || tool.Error {
		t.Fatalf("tool = %#v, want list_dir with its result", tool)
	}
	if page.Entries[0].Timestamp != "2027-01-15T08:00:02.25Z" {
		t.Fatalf("timestamp = %q, want agentTimestampMs in RFC3339", page.Entries[0].Timestamp)
	}
	seen := map[string]bool{}
	for _, entry := range page.Entries {
		if entry.ID == "" || seen[entry.ID] {
			t.Fatalf("entry IDs = %#v, want unique non-empty IDs", page.Entries)
		}
		seen[entry.ID] = true
	}
	again := readGrok(t, reader, "", 80)
	for index := range page.Entries {
		if again.Entries[index].ID != page.Entries[index].ID {
			t.Fatal("entry IDs must be stable across reads")
		}
	}
}

func TestGrokKeepsHistoryBeforeCompaction(t *testing.T) {
	reader, home := testReader(t)
	f := &grokFixture{}
	f.user(0, "first question")
	f.agent("first answer")
	f.other("turn_completed")
	f.other("auto_compact_started")
	f.other("compaction_checkpoint")
	f.other("auto_compact_completed")
	f.user(1, "second question")
	f.agent("second answer")
	f.write(t, grokUpdatesPath(home, grokCWD, testSessionID))

	page := readGrok(t, reader, "", 80)
	if len(page.Entries) != 4 {
		t.Fatalf("entries = %#v, want the turns on both sides of the compaction", page.Entries)
	}
	assertEntry(t, page.Entries[0], "user", "first question", 0)
	assertEntry(t, page.Entries[1], "assistant", "first answer", 0)
	assertEntry(t, page.Entries[2], "user", "second question", 0)
	assertEntry(t, page.Entries[3], "assistant", "second answer", 0)
}

func TestGrokMessageSentWhileWorking(t *testing.T) {
	reader, home := testReader(t)
	f := &grokFixture{}
	f.user(0, "run the tests")
	f.agent("Running them.")
	f.toolCall("call-1", "run_terminal_command", map[string]any{"command": "go test"})
	f.user(1, "The user sent a message while you were working:\n<user_query>\nalso run vet\n</user_query>")
	f.toolResult("call-1", "completed", "ok")
	f.agent("Tests pass; running vet next.")
	f.write(t, grokUpdatesPath(home, grokCWD, testSessionID))

	page := readGrok(t, reader, "", 80)
	if len(page.Entries) != 4 {
		t.Fatalf("entries = %#v, want 4", page.Entries)
	}
	assertEntry(t, page.Entries[0], "user", "run the tests", 0)
	assertEntry(t, page.Entries[1], "assistant", "Running them.", 1)
	assertEntry(t, page.Entries[2], "user", "also run vet", 0)
	assertEntry(t, page.Entries[3], "assistant", "Tests pass; running vet next.", 0)
	if page.Entries[1].Tools[0].Output != "ok" {
		t.Fatalf("tool = %#v, want result attached to the earlier call", page.Entries[1].Tools[0])
	}
}

func TestGrokParallelToolResultsOutOfOrderAndFailure(t *testing.T) {
	reader, home := testReader(t)
	f := &grokFixture{}
	f.user(0, "read two files")
	f.toolCall("call-a", "read_file", map[string]any{"path": "a.go"})
	f.toolCall("call-b", "read_file", map[string]any{"path": "b.go"})
	f.toolCall("call-c", "search_replace", map[string]any{"path": "c.go"})
	f.toolResult("call-b", "completed", "contents of b")
	f.toolDiff("call-c")
	f.toolResult("call-a", "failed", "file not found")
	f.toolResult("call-unknown", "completed", "orphan")
	f.write(t, grokUpdatesPath(home, grokCWD, testSessionID))

	page := readGrok(t, reader, "", 80)
	if len(page.Entries) != 2 {
		t.Fatalf("entries = %#v, want user and one tool entry", page.Entries)
	}
	assertEntry(t, page.Entries[1], "assistant", "", 3)
	tools := page.Entries[1].Tools
	if tools[0].ID != "call-a" || tools[0].Output != "file not found" || !tools[0].Error {
		t.Fatalf("tool a = %#v, want failed result", tools[0])
	}
	if tools[1].ID != "call-b" || tools[1].Output != "contents of b" || tools[1].Error {
		t.Fatalf("tool b = %#v, want completed result", tools[1])
	}
	if tools[2].Output != `{"EditsApplied":1,"type":"EditsApplied"}` || tools[2].Error {
		t.Fatalf("tool c = %#v, want rawOutput when no text block exists", tools[2])
	}
}

func TestGrokUserChunks(t *testing.T) {
	reader, home := testReader(t)
	f := &grokFixture{}
	f.hostTurn("/always-approve")
	f.userImage(0)
	f.user(0, "describe the screenshot")
	f.other("image_compressed")
	f.user(0, "and keep it short")
	f.agent("It shows a form.")
	f.hiddenUser(1, "<system-reminder>background task finished</system-reminder>")
	f.agent("The task finished.")
	f.user(2, "untagged follow-up without a wrapper")
	f.write(t, grokUpdatesPath(home, grokCWD, testSessionID))

	page := readGrok(t, reader, "", 80)
	if len(page.Entries) != 5 {
		t.Fatalf("entries = %#v, want 5", page.Entries)
	}
	assertEntry(t, page.Entries[0], "user", "/always-approve", 0)
	assertEntry(t, page.Entries[1], "user", "describe the screenshot\nand keep it short", 0)
	assertEntry(t, page.Entries[2], "assistant", "It shows a form.", 0)
	assertEntry(t, page.Entries[3], "assistant", "The task finished.", 0)
	assertEntry(t, page.Entries[4], "user", "untagged follow-up without a wrapper", 0)
}

func TestGrokCorruptAndPartialRecords(t *testing.T) {
	reader, home := testReader(t)
	path := grokUpdatesPath(home, grokCWD, testSessionID)
	normalGrokSession().write(t, path)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.WriteString(`{"params":{"update":{"sessionUpdate":"agent_message_chunk","content":` + "\n")
	_, _ = file.WriteString(`{"params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"partial`)
	_ = file.Close()

	page := readGrok(t, reader, "", 80)
	if len(page.Entries) != 3 || !page.SourceCorrupt {
		t.Fatalf("page = %#v, want valid entries kept and corruption reported", page)
	}
}

func TestGrokParseHonoursCancellation(t *testing.T) {
	_, home := testReader(t)
	path := grokUpdatesPath(home, grokCWD, testSessionID)
	normalGrokSession().write(t, path)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := parseGrokUpdates(ctx, path); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func grokTurns(count int) *grokFixture {
	f := &grokFixture{}
	for turn := 0; turn < count; turn++ {
		f.user(turn, fmt.Sprintf("question %d", turn))
		f.agent(fmt.Sprintf("answer %d", turn))
	}
	return f
}

func TestGrokReaderPaginatesWithBefore(t *testing.T) {
	reader, home := testReader(t)
	grokTurns(3).write(t, grokUpdatesPath(home, grokCWD, testSessionID))

	latest := readGrok(t, reader, "", 2)
	if len(latest.Entries) != 2 || !latest.HasMore || latest.Total != 6 || latest.Entries[0].Text != "question 2" {
		t.Fatalf("latest = %#v", latest)
	}
	older := readGrok(t, reader, latest.Entries[0].ID, 4)
	if len(older.Entries) != 4 || older.HasMore || older.Entries[0].Text != "question 0" || older.Entries[3].Text != "answer 1" {
		t.Fatalf("older = %#v", older)
	}
	for _, before := range []string{"999999", "not-a-cursor"} {
		if page := readGrok(t, reader, before, 2); page.Available || page.ReasonCode != "invalid_cursor" {
			t.Fatalf("before %q page = %#v, want invalid_cursor", before, page)
		}
	}
}

func TestGrokBrowserPaginatesAndRejectsReplacedSource(t *testing.T) {
	reader, home := testReader(t)
	path := grokUpdatesPath(home, grokCWD, testSessionID)
	grokTurns(3).write(t, path)
	browser, err := NewBrowser(reader, t.TempDir(), DefaultBrowserOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "grok", CWD: grokCWD, SessionID: testSessionID}

	latest, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 2})
	if err != nil || latest.Mode != BrowseNative || len(latest.Entries) != 2 || !latest.HasMore || latest.NextCursor == "" ||
		latest.Entries[1].Text != "answer 2" || latest.Total == nil || *latest.Total != 6 {
		t.Fatalf("latest = %#v, err = %v", latest, err)
	}
	older, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: latest.NextCursor, Limit: 2})
	if err != nil || len(older.Entries) != 2 || older.Entries[0].Text != "question 1" || !older.HasMore {
		t.Fatalf("older = %#v, err = %v", older, err)
	}

	replacement := path + ".new"
	grokTurns(3).write(t, replacement)
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	stale, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: older.NextCursor, Limit: 2})
	if err != nil || stale.Error == nil || stale.ReasonCode != "source_changed" {
		t.Fatalf("stale = %#v, err = %v, want source_changed", stale, err)
	}
	fresh, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 2})
	if err != nil || !fresh.Available || fresh.Error != nil || len(fresh.Entries) != 2 {
		t.Fatalf("fresh = %#v, err = %v", fresh, err)
	}
}

func TestGrokProjectDirNameMatchesURLEncoding(t *testing.T) {
	cases := map[string]string{
		"/Users/dev":                   "%2FUsers%2Fdev",
		"/work/a b/x+y@z":              "%2Fwork%2Fa%20b%2Fx%2By%40z",
		"/work/café":                   "%2Fwork%2Fcaf%C3%A9",
		"/tmp/keep-_.~chars":           "%2Ftmp%2Fkeep-_.~chars",
		"/tmp/grok-proxy-check-qx_s8f": "%2Ftmp%2Fgrok-proxy-check-qx_s8f",
	}
	for cwd, want := range cases {
		if got := grokProjectDirName(cwd); got != want {
			t.Fatalf("grokProjectDirName(%q) = %q, want %q", cwd, got, want)
		}
	}
}

func TestGrokLocateUsesEncodedAndRealProjectDirectory(t *testing.T) {
	reader, home := testReader(t)
	cwd := "/work/x+y@z/café"
	path := grokUpdatesPath(home, cwd, testSessionID)
	grokTurns(1).write(t, path)
	want, _ := filepath.EvalSymlinks(path)
	if location := reader.Locate("grok", cwd, testSessionID); location.Path != want {
		t.Fatalf("Locate(%q) = %q, want %q", cwd, location.Path, want)
	}

	realDir := filepath.Join(t.TempDir(), "real")
	if err := os.MkdirAll(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatal(err)
	}
	resolved, _ := filepath.EvalSymlinks(link)
	names := grokProjectDirNames(link)
	if len(names) != 2 || names[1] != grokProjectDirName(resolved) {
		t.Fatalf("grokProjectDirNames(%q) = %v, want the real path encoding too", link, names)
	}
	const linkedSession = "grok-linked-session"
	linkedPath := grokUpdatesPath(home, resolved, linkedSession)
	grokTurns(1).write(t, linkedPath)
	wantLinked, _ := filepath.EvalSymlinks(linkedPath)
	if location := reader.Locate("grok", link, linkedSession); location.Path != wantLinked {
		t.Fatalf("Locate(symlink) = %q, want %q", location.Path, wantLinked)
	}
}

func TestGrokLocateFallsBackToSessionScan(t *testing.T) {
	reader, home := testReader(t)
	path := grokUpdatesPath(home, "/somewhere/else", testSessionID)
	grokTurns(1).write(t, path)

	for _, cwd := range []string{"", grokCWD} {
		location := reader.Locate("grok", cwd, testSessionID)
		want, _ := filepath.EvalSymlinks(path)
		if location.Path != want {
			t.Fatalf("Locate(cwd=%q) = %q, want %q", cwd, location.Path, want)
		}
	}
}

func TestGrokLocateIgnoresChatHistoryOnly(t *testing.T) {
	reader, home := testReader(t)
	legacy := filepath.Join(filepath.Dir(grokUpdatesPath(home, grokCWD, testSessionID)), "chat_history.jsonl")
	writeRows(t, legacy, map[string]any{"type": "user", "content": "synthetic"})
	if location := reader.Locate("grok", grokCWD, testSessionID); location.Path != "" {
		t.Fatalf("Locate = %q, want no location without updates.jsonl", location.Path)
	}
}

func TestGrokLocateRejectsUnsafeSessionIDs(t *testing.T) {
	reader, home := testReader(t)
	grokTurns(1).write(t, grokUpdatesPath(home, grokCWD, testSessionID))

	for _, sessionID := range []string{"", "..", "../" + testSessionID, "a/b"} {
		if location := reader.Locate("grok", grokCWD, sessionID); location.Path != "" {
			t.Fatalf("Locate(%q) = %q, want no location", sessionID, location.Path)
		}
	}
}

func TestGrokSessionTitleFromSummary(t *testing.T) {
	long := strings.Repeat("t", maxGrokTitleBytes+50)
	cases := []struct{ body, want string }{
		{`{"session_summary":"Summary","generated_title":"Synthetic title"}`, "Synthetic title"},
		{`{"session_summary":"Summary","generated_title":""}`, "Summary"},
		{`{"generated_title":"` + long + `"}`, long[:maxGrokTitleBytes]},
	}
	for _, test := range cases {
		reader, home := testReader(t)
		path := grokUpdatesPath(home, grokCWD, testSessionID)
		grokTurns(1).write(t, path)
		summary := filepath.Join(filepath.Dir(path), "summary.json")
		if err := os.WriteFile(summary, []byte(test.body), 0o600); err != nil {
			t.Fatal(err)
		}
		title := reader.Locate("grok", grokCWD, testSessionID).Title
		if title != test.want {
			t.Fatalf("summary %s: title = %q, want %q", test.body, title, test.want)
		}
	}
}

func TestGrokRelevantLineSkipsOtherKinds(t *testing.T) {
	for _, line := range []string{"", `{"params":{"update":{"sessionUpdate":"hook_execution"}}}`, `{"params":{"update":{"sessionUpdate":"agent_thought_chunk"}}}`} {
		if grokRelevantLine([]byte(line)) {
			t.Fatalf("grokRelevantLine(%q) = true", line)
		}
	}
	if !grokRelevantLine([]byte(strings.Repeat(" ", 3) + `{"sessionUpdate":"tool_call_update"}`)) {
		t.Fatal("tool_call_update should be relevant")
	}
}
