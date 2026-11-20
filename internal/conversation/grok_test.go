package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
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
	switch update["sessionUpdate"] {
	case "rewind_marker", "turn_completed", "hook_execution", "auto_compact_started", "auto_compact_completed", "compaction_checkpoint", "image_compressed":
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
		"rawOutput": map[string]any{"type": "SearchReplace", "EditsApplied": map[string]any{"tool_output_for_prompt": "Applied edit to /work/a.go", "absolute_path": "/work/a.go", "edits": []any{map[string]any{"old_string": "a", "new_string": "b"}}, "patch": "-a\n+b"}},
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

func readGrok(t *testing.T, reader *Reader, limit int) []Entry {
	t.Helper()
	entries, err := reader.ReadLatest("grok", ProjectContext{CWD: grokCWD}, testSessionID, limit)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func readGrokBrowser(t *testing.T, reader *Reader, limit int) BrowsePage {
	t.Helper()
	browser := newTestBrowser(t, reader, DefaultBrowserOptions())
	scope := BrowseScope{Provider: "grok", CWD: grokCWD, SessionID: testSessionID}
	page, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: scope, Limit: limit})
	if err != nil || !page.Available {
		t.Fatalf("browser page = %#v, err = %v", page, err)
	}
	return page
}

func grokBrowserCorrupt(t *testing.T, reader *Reader) bool {
	t.Helper()
	return readGrokBrowser(t, reader, 80).Diagnostics.CorruptRecords > 0
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

func TestGrokCLIIntegration(t *testing.T) {
	if os.Getenv("HERDR_GROK_INTEGRATION") != "1" {
		t.Skip("set HERDR_GROK_INTEGRATION=1 for an isolated live Grok exchange")
	}
	const binary = "/Users/christophe.vidal/.grok/bin/grok"
	if info, err := os.Stat(binary); err != nil || !info.Mode().IsRegular() {
		t.Fatal("installed Grok executable is unavailable")
	}
	home, err := os.MkdirTemp("", "grok-it-")
	if err != nil {
		t.Fatal("cannot create isolated Grok home")
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	nativeHome := filepath.Dir(filepath.Dir(binary))
	work := filepath.Join(home, "work")
	state := filepath.Join(home, ".grok")
	for _, dir := range []string{work, state, filepath.Join(home, "tmp")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal("cannot create isolated Grok directories")
		}
	}
	if err := os.WriteFile(filepath.Join(state, "config.toml"), []byte("[cli]\nuse_leader = false\n[subagents]\nenabled = false\n[workflows]\nenabled = false\n"), 0o600); err != nil {
		t.Fatal("cannot configure isolated Grok state")
	}
	sandbox := fmt.Sprintf("[profiles.integration]\nextends = \"strict\"\nread_write = [%q]\n", nativeHome)
	if err := os.WriteFile(filepath.Join(state, "sandbox.toml"), []byte(sandbox), 0o600); err != nil {
		t.Fatal("cannot configure native authentication access")
	}
	environment := []string{
		"PATH=/usr/bin:/bin", "LANG=en_US.UTF-8", "HOME=" + home,
		"GROK_HOME=" + state, "TMPDIR=" + filepath.Join(home, "tmp"),
		"GROK_AUTH_PATH=" + filepath.Join(nativeHome, "auth.json"),
		"GROK_LEADER_SOCKET=" + filepath.Join(state, "leader.sock"),
		"XDG_CONFIG_HOME=" + filepath.Join(home, "config"),
		"XDG_CACHE_HOME=" + filepath.Join(home, "cache"),
		"XDG_DATA_HOME=" + filepath.Join(home, "data"),
		"XDG_STATE_HOME=" + filepath.Join(home, "state"),
		"GROK_BACKEND_SEARCH=0", "GROK_WORKFLOWS=0", "GROK_MEMORY=0",
		"GROK_CAMPAIGNS=0", "DISABLE_TELEMETRY=1", "DISABLE_ERROR_REPORTING=1",
		"GROK_TELEMETRY_TRACE_UPLOAD=0", "GROK_EXTERNAL_OTEL=0",
	}
	run := func(timeout time.Duration, args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(t.Context(), timeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Dir, cmd.Env = work, environment
		cmd.WaitDelay = time.Second
		return cmd.Output()
	}
	help, err := run(10*time.Second, "--help")
	if err != nil {
		t.Fatal("isolated Grok help failed; command output withheld")
	}
	version, err := run(10*time.Second, "--version")
	if err != nil {
		t.Fatal("isolated Grok version query failed; command output withheld")
	}
	versionNumber := regexp.MustCompile(`\b[0-9]+\.[0-9]+\.[0-9]+\b`).Find(version)
	if len(versionNumber) == 0 {
		t.Fatal("Grok did not report a recognizable version")
	}
	t.Logf("installed CLI version: %s", versionNumber)
	for _, flag := range []string{"--sandbox", "--tools", "--deny", "--max-turns", "--output-format"} {
		if !strings.Contains(string(help), flag) {
			t.Fatalf("required isolated headless option %s is not advertised; no prompt sent", flag)
		}
	}
	const prompt = "Reply with exactly HERDR_GROK_DISPOSABLE_CHECK. Do not use tools."
	output, err := run(90*time.Second, "--sandbox", "integration", "--no-auto-update", "-p", prompt,
		"--tools", "read_file", "--deny", "Read", "--deny", "MCPTool",
		"--max-turns", "1", "--output-format", "json")
	if err != nil {
		t.Fatal("isolated live exchange failed or timed out; command output withheld")
	}
	t.Setenv("GROK_HOME", state)
	t.Setenv("HERDR_GROK_CONFIG_DIRS", "")
	reader := NewReader(home)
	readGenerated := func(output []byte) ([]Entry, string, string) {
		t.Helper()
		var response struct {
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(output, &response) != nil || !safeSessionID(response.SessionID) {
			t.Fatal("live exchange did not report a valid new session ID")
		}
		location := reader.LocateWithProject("grok", ProjectContext{CWD: work}, response.SessionID)
		if location.Path == "" || containedRegularFile(location.Path, state) == "" {
			t.Fatal("production lookup did not locate the isolated live updates.jsonl")
		}
		entries, err := reader.ReadLatest("grok", ProjectContext{CWD: work}, response.SessionID, 80)
		scope := BrowseScope{Provider: "grok", CWD: work, SessionID: response.SessionID}
		browsed, browseErr := newTestBrowser(t, reader, DefaultBrowserOptions()).ReadPage(t.Context(), BrowseRequest{Scope: scope, Limit: 80})
		if err != nil || browseErr != nil || browsed.Diagnostics.CorruptRecords > 0 {
			t.Fatal("production reader did not parse the isolated live history")
		}
		return entries, location.Path, response.SessionID
	}
	entries, _, firstSession := readGenerated(output)
	user, assistant := false, false
	for _, entry := range entries {
		user = user || entry.Role == "user" && strings.Contains(entry.Text, "HERDR_GROK_DISPOSABLE_CHECK")
		assistant = assistant || entry.Role == "assistant" && strings.TrimSpace(entry.Text) != ""
		if len(entry.Tools) != 0 {
			t.Fatal("unexpected tool activity in the no-tool live exchange")
		}
	}
	if !user || !assistant {
		t.Fatal("live history lacks the disposable user prompt or assistant response")
	}
	fixture := filepath.Join(work, "fixture.txt")
	if err := os.WriteFile(fixture, []byte("HERDR_GROK_BEFORE\n"), 0o600); err != nil {
		t.Fatal("cannot create disposable edit fixture")
	}
	editPrompt := "The entire content of " + fixture + " is HERDR_GROK_BEFORE followed by a newline. Use search_replace to replace HERDR_GROK_BEFORE with HERDR_GROK_AFTER in that file. Read tools are denied; do not use them or any other tool. Then reply HERDR_GROK_EDIT_DONE."
	output, err = run(90*time.Second, "--sandbox", "integration", "--no-auto-update", "-p", editPrompt,
		"--tools", "read_file,search_replace", "--allow", "Edit("+fixture+")", "--deny", "Read", "--deny", "MCPTool",
		"--max-turns", "3", "--output-format", "json")
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			t.Logf("edit process exit status: %d", exit.ExitCode())
			for _, category := range []string{"unexpected argument", "invalid value", "sandbox", "permission", "auth", "socket", "config", "not signed in", "tool", "model", "rate limit"} {
				if strings.Contains(strings.ToLower(string(exit.Stderr)), category) {
					t.Logf("edit stderr diagnostic category: %s", category)
				}
			}
		}
		t.Fatal("isolated edit exchange failed or timed out; command output withheld")
	}
	entries, path, editSession := readGenerated(output)
	if editSession == firstSession {
		t.Fatal("edit exchange reused a session")
	}
	data, err := os.ReadFile(fixture)
	if err != nil || string(data) != "HERDR_GROK_AFTER\n" {
		t.Fatal("disposable edit was not applied exactly")
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal("cannot inspect generated update event families")
	}
	families := map[string]bool{}
	editOutput := ""
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record struct {
			Method string `json:"method"`
			Params struct {
				Update grokUpdate `json:"update"`
			} `json:"params"`
		}
		if json.Unmarshal([]byte(line), &record) != nil {
			t.Fatal("generated update is not valid JSON")
		}
		kind := record.Params.Update.SessionUpdate
		expected := ""
		switch kind {
		case "user_message_chunk", "agent_message_chunk", "tool_call", "tool_call_update":
			expected = "session/update"
		case "turn_completed", "hook_execution", "rewind_marker":
			expected = "_x.ai/session/update"
		}
		if expected != "" {
			if record.Method != expected {
				t.Fatalf("unexpected method family for %s", kind)
			}
			families[kind] = true
		}
		var raw struct {
			Type         string `json:"type"`
			EditsApplied struct {
				Output string `json:"tool_output_for_prompt"`
			} `json:"EditsApplied"`
		}
		if json.Unmarshal(record.Params.Update.RawOutput, &raw) == nil && raw.Type == "SearchReplace" {
			if raw.EditsApplied.Output != "" {
				editOutput = sanitizeText(raw.EditsApplied.Output)
			}
		}
	}
	if editOutput == "" {
		t.Fatal("generated edit did not contain SearchReplace.EditsApplied.tool_output_for_prompt")
	}
	matched := false
	for _, entry := range entries {
		for _, tool := range entry.Tools {
			matched = matched || !tool.Error && tool.Output == editOutput
		}
	}
	if !matched {
		t.Fatal("production projection did not preserve the actual edit prompt output")
	}
	for _, kind := range []string{"user_message_chunk", "agent_message_chunk", "tool_call", "tool_call_update", "turn_completed"} {
		if !families[kind] {
			t.Fatalf("generated edit session lacks %s", kind)
		}
		t.Logf("observed expected method family for %s", kind)
	}
	t.Log("validated generated user/assistant history and disposable SearchReplace output; no live image, rewind or interjection reproduction")
}

func TestGrokIsSupported(t *testing.T) {
	if !Supported("grok") || !Supported("Grok") {
		t.Fatal("grok should be a supported conversation provider")
	}
}

func TestGrokReadsNormalSession(t *testing.T) {
	reader, home := testReader(t)
	normalGrokSession().write(t, grokUpdatesPath(home, grokCWD, testSessionID))

	page := readGrok(t, reader, 80)
	browsed := readGrokBrowser(t, reader, 80)
	if len(page) != 3 || browsed.Total == nil || *browsed.Total != 3 || browsed.HasMore || browsed.Diagnostics.CorruptRecords != 0 {
		t.Fatalf("page = %#v, browsed = %#v, want 3 available entries", page, browsed)
	}
	assertEntry(t, page[0], "user", "list the files", 0)
	assertEntry(t, page[1], "assistant", "I'll check.", 1)
	assertEntry(t, page[2], "assistant", "There are two files.", 0)
	tool := page[1].Tools[0]
	if tool.ID != "call-1" || tool.Name != "list_dir" || tool.Input != `{"path":"."}` || tool.Output != "a.go\nb.go" || tool.Error {
		t.Fatalf("tool = %#v, want list_dir with its result", tool)
	}
	if page[0].Timestamp != "2027-01-15T08:00:02.25Z" {
		t.Fatalf("timestamp = %q, want agentTimestampMs in RFC3339", page[0].Timestamp)
	}
	seen := map[string]bool{}
	for _, entry := range page {
		if entry.ID == "" || seen[entry.ID] {
			t.Fatalf("entry IDs = %#v, want unique non-empty IDs", page)
		}
		seen[entry.ID] = true
	}
	again := readGrok(t, reader, 80)
	for index := range page {
		if again[index].ID != page[index].ID {
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

	page := readGrok(t, reader, 80)
	if len(page) != 4 {
		t.Fatalf("entries = %#v, want the turns on both sides of the compaction", page)
	}
	assertEntry(t, page[0], "user", "first question", 0)
	assertEntry(t, page[1], "assistant", "first answer", 0)
	assertEntry(t, page[2], "user", "second question", 0)
	assertEntry(t, page[3], "assistant", "second answer", 0)
}

func TestGrokMessageSentWhileWorking(t *testing.T) {
	reader, home := testReader(t)
	f := &grokFixture{}
	f.user(0, "run the tests")
	f.agent("Running them.")
	f.toolCall("call-1", "run_terminal_command", map[string]any{"command": "go test"})
	f.add(map[string]any{
		"sessionUpdate": "user_message_chunk",
		"content":       map[string]any{"type": "text", "text": "The user sent a message while you were working:\n<user_query>\nalso run vet\n</user_query>"},
		"_meta":         map[string]any{"promptIndex": 1, "interjection": true},
	})
	f.toolResult("call-1", "completed", "ok")
	f.agent("Tests pass; running vet next.")
	f.write(t, grokUpdatesPath(home, grokCWD, testSessionID))

	page := readGrok(t, reader, 80)
	if len(page) != 4 {
		t.Fatalf("entries = %#v, want 4", page)
	}
	assertEntry(t, page[0], "user", "run the tests", 0)
	assertEntry(t, page[1], "assistant", "Running them.", 1)
	assertEntry(t, page[2], "user", "also run vet", 0)
	assertEntry(t, page[3], "assistant", "Tests pass; running vet next.", 0)
	if page[1].Tools[0].Output != "ok" {
		t.Fatalf("tool = %#v, want result attached to the earlier call", page[1].Tools[0])
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

	page := readGrok(t, reader, 80)
	if len(page) != 2 {
		t.Fatalf("entries = %#v, want user and one tool entry", page)
	}
	assertEntry(t, page[1], "assistant", "", 3)
	tools := page[1].Tools
	if tools[0].ID != "call-a" || tools[0].Output != "file not found" || !tools[0].Error {
		t.Fatalf("tool a = %#v, want failed result", tools[0])
	}
	if tools[1].ID != "call-b" || tools[1].Output != "contents of b" || tools[1].Error {
		t.Fatalf("tool b = %#v, want completed result", tools[1])
	}
	if tools[2].Output != "Applied edit to /work/a.go" || tools[2].Error {
		t.Fatalf("tool c = %#v, want rawOutput when no text block exists", tools[2])
	}
	expectPreview(t, "grok", tools[2], nil)
}

func TestGrokUserQueryMarkup(t *testing.T) {
	const prefix = "The user sent a message while you were working:\n<user_query>"
	const suffix = "</user_query>"
	for _, tagged := range []bool{false, true} {
		for _, tc := range []struct {
			name         string
			text         string
			interjection bool
			want         string
		}{
			{name: "plain", text: "hello"},
			{name: "literal", text: "Explain <user_query>hello</user_query> and preserve this suffix"},
			{name: "multiple", text: "<user_query>first</user_query> and <user_query>second</user_query>"},
			{name: "nested", text: "<user_query>outer <user_query>inner</user_query> end</user_query>"},
			{name: "wrapper without metadata", text: prefix + "hello" + suffix},
			{name: "interjection", text: prefix + "\nhello\n" + suffix, interjection: true, want: "hello"},
			{name: "interjection literal markup", text: prefix + "Explain <user_query>outer <user_query>inner</user_query></user_query> and <user_query>second</user_query> suffix" + suffix, interjection: true, want: "Explain <user_query>outer <user_query>inner</user_query></user_query> and <user_query>second</user_query> suffix"},
			{name: "missing close", text: prefix + "hello", interjection: true},
			{name: "missing open", text: "hello" + suffix, interjection: true},
			{name: "unknown prefix", text: "Notice: <user_query>hello</user_query>", interjection: true},
			{name: "trailing text", text: prefix + "hello" + suffix + " preserve", interjection: true},
		} {
			t.Run(fmt.Sprintf("tagged=%t/%s", tagged, tc.name), func(t *testing.T) {
				f := &grokFixture{}
				meta := map[string]any{"interjection": tc.interjection}
				if tagged {
					meta["promptIndex"] = 0
				}
				f.add(map[string]any{
					"sessionUpdate": "user_message_chunk",
					"content":       map[string]any{"type": "text", "text": tc.text},
					"_meta":         meta,
				})
				reader, home := testReader(t)
				f.write(t, grokUpdatesPath(home, grokCWD, testSessionID))
				want := tc.want
				if want == "" {
					want = tc.text
				}
				assertGrokTexts(t, readGrok(t, reader, 80), "user:"+want)
			})
		}
		t.Run(fmt.Sprintf("tagged=%t/chunk joins", tagged), func(t *testing.T) {
			f := &grokFixture{}
			for _, text := range []string{"Explain <user_query>hello</user_query>", "and preserve this suffix"} {
				if tagged {
					f.user(0, text)
				} else {
					f.untaggedUser(text)
				}
			}
			reader, home := testReader(t)
			f.write(t, grokUpdatesPath(home, grokCWD, testSessionID))
			assertGrokTexts(t, readGrok(t, reader, 80), "user:Explain <user_query>hello</user_query>\nand preserve this suffix")
		})
	}
}

func TestGrokUserChunks(t *testing.T) {
	reader, home := testReader(t)
	f := &grokFixture{}
	f.hostTurn("/always-approve")
	f.other("image_compressed")
	f.userImage(0)
	f.user(0, "describe the screenshot")
	f.user(0, "and keep it short")
	f.agent("It shows a form.")
	f.hiddenUser(1, "<system-reminder>background task finished</system-reminder>")
	f.agent("The task finished.")
	f.user(2, "untagged follow-up without a wrapper")
	f.write(t, grokUpdatesPath(home, grokCWD, testSessionID))

	page := readGrok(t, reader, 80)
	if len(page) != 5 {
		t.Fatalf("entries = %#v, want 5", page)
	}
	assertEntry(t, page[0], "user", "/always-approve", 0)
	assertEntry(t, page[1], "user", "describe the screenshot\nand keep it short", 0)
	assertEntry(t, page[2], "assistant", "It shows a form.", 0)
	assertEntry(t, page[3], "assistant", "The task finished.", 0)
	assertEntry(t, page[4], "user", "untagged follow-up without a wrapper", 0)
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

	page := readGrok(t, reader, 80)
	if len(page) != 3 || !grokBrowserCorrupt(t, reader) {
		t.Fatalf("page = %#v, want valid entries kept and corruption reported", page)
	}
}

func TestGrokParseHonoursCancellation(t *testing.T) {
	reader, home := testReader(t)
	path := grokUpdatesPath(home, grokCWD, testSessionID)
	normalGrokSession().write(t, path)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := parseGrokUpdates(ctx, reader.LocateWithProject("grok", ProjectContext{CWD: grokCWD}, testSessionID)); !errors.Is(err, context.Canceled) {
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

func TestGrokBrowserPaginatesAndRejectsReplacedSource(t *testing.T) {
	reader, home := testReader(t)
	path := grokUpdatesPath(home, grokCWD, testSessionID)
	grokTurns(3).write(t, path)
	browser := newTestBrowser(t, reader, DefaultBrowserOptions())
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

func TestGrokBrowserRejectsInvalidCursor(t *testing.T) {
	reader, home := testReader(t)
	grokTurns(3).write(t, grokUpdatesPath(home, grokCWD, testSessionID))
	browser := newTestBrowser(t, reader, DefaultBrowserOptions())
	scope := BrowseScope{Provider: "grok", CWD: grokCWD, SessionID: testSessionID}
	latest, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 2})
	if err != nil || latest.SourceRevision == "" {
		t.Fatalf("latest = %#v, err = %v", latest, err)
	}
	for _, before := range []string{"999999", "not-a-cursor"} {
		cursor := signedNativeCursor(t, browser, scope, latest.SourceRevision, before)
		page, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: cursor, Limit: 2})
		if err != nil || page.State != BrowseFailed || page.ReasonCode != "invalid_cursor" {
			t.Fatalf("before %q page = %#v, err = %v, want invalid_cursor", before, page, err)
		}
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
	if location := reader.LocateWithProject("grok", ProjectContext{CWD: cwd}, testSessionID); location.Path != want {
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
	if location := reader.LocateWithProject("grok", ProjectContext{CWD: link}, linkedSession); location.Path != wantLinked {
		t.Fatalf("Locate(symlink) = %q, want %q", location.Path, wantLinked)
	}
}

func TestGrokLocateFallsBackToSessionScan(t *testing.T) {
	reader, home := testReader(t)
	path := grokUpdatesPath(home, "/somewhere/else", testSessionID)
	grokTurns(1).write(t, path)

	location := reader.LocateWithProject("grok", ProjectContext{CWD: ""}, testSessionID)
	want, _ := filepath.EvalSymlinks(path)
	if location.Path != want {
		t.Fatalf("Locate without cwd = %q, want %q", location.Path, want)
	}
}

func TestGrokLocateRejectsOtherProject(t *testing.T) {
	reader, home := testReader(t)
	grokTurns(1).write(t, grokUpdatesPath(home, "/other/project", testSessionID))
	if location := reader.LocateWithProject("grok", ProjectContext{CWD: grokCWD}, testSessionID); location.Path != "" {
		t.Fatalf("cross-project location = %#v", location)
	}
}

func TestGrokLocateIgnoresChatHistoryOnly(t *testing.T) {
	reader, home := testReader(t)
	legacy := filepath.Join(filepath.Dir(grokUpdatesPath(home, grokCWD, testSessionID)), "chat_history.jsonl")
	writeRows(t, legacy, map[string]any{"type": "user", "content": "synthetic"})
	if location := reader.LocateWithProject("grok", ProjectContext{CWD: grokCWD}, testSessionID); location.Path != "" {
		t.Fatalf("Locate = %q, want no location without updates.jsonl", location.Path)
	}
}

func TestGrokLocateRejectsUnsafeSessionIDs(t *testing.T) {
	reader, home := testReader(t)
	grokTurns(1).write(t, grokUpdatesPath(home, grokCWD, testSessionID))

	for _, sessionID := range []string{"", "..", "../" + testSessionID, "a/b"} {
		if location := reader.LocateWithProject("grok", ProjectContext{CWD: grokCWD}, sessionID); location.Path != "" {
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
		title := reader.LocateWithProject("grok", ProjectContext{CWD: grokCWD}, testSessionID).Title
		if title != test.want {
			t.Fatalf("summary %s: title = %q, want %q", test.body, title, test.want)
		}
	}
}
