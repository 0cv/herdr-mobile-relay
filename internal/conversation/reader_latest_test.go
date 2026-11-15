package conversation

import (
	"path/filepath"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
)

type latestReplyWant struct {
	text       string
	timestamp  string
	toolName   string
	toolOutput string
}

func assertLatestReply(t *testing.T, page Page, err error, want latestReplyWant) {
	t.Helper()
	if err != nil || !page.Available || len(page.Entries) < 2 {
		t.Fatalf("page = %#v, err = %v, want a prompt and a reply", page, err)
	}
	entry := page.Entries[len(page.Entries)-1]
	if entry.Role != "assistant" || entry.Text != want.text || entry.Timestamp != want.timestamp {
		t.Fatalf("latest entry = %#v, want assistant %q at %q", entry, want.text, want.timestamp)
	}
	if len(entry.Tools) != 1 || entry.Tools[0].Name != want.toolName || entry.Tools[0].Output != want.toolOutput {
		t.Fatalf("latest tools = %#v, want %s -> %q", entry.Tools, want.toolName, want.toolOutput)
	}
	if page.Entries[0].Role != "user" {
		t.Fatalf("first entry = %#v, want the user prompt", page.Entries[0])
	}
}

func TestReadWithProjectLatestEntriesForJSONLProviders(t *testing.T) {
	const cwd = "/work"
	piMessages := func() []map[string]any {
		return []map[string]any{
			{"type": "message", "id": "u1", "timestamp": "2026-08-12T10:00:00Z", "message": map[string]any{"role": "user", "content": "question"}},
			{"type": "message", "id": "a1", "timestamp": "2026-08-12T10:00:01Z", "message": map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": "latest reply"},
				map[string]any{"type": "toolCall", "id": "call-1", "name": "read", "arguments": map[string]any{"path": "a.go"}},
			}}},
			{"type": "message", "id": "t1", "timestamp": "2026-08-12T10:00:02Z", "message": map[string]any{"role": "toolResult", "toolCallId": "call-1", "content": "file body"}},
		}
	}
	tests := []struct {
		agent string
		write func(t *testing.T, home string) string
		want  latestReplyWant
	}{
		{
			agent: "pi",
			write: func(t *testing.T, home string) string {
				path := filepath.Join(home, ".pi", "agent", "sessions", "--work--", "session.jsonl")
				writeRows(t, path, piMessages()...)
				return path
			},
			want: latestReplyWant{"latest reply", "2026-08-12T10:00:01Z", "read", "file body"},
		},
		{
			agent: "omp",
			write: func(t *testing.T, home string) string {
				writeRows(t, filepath.Join(home, ".omp", "agent", "sessions", "-work", "session_"+testSessionID+".jsonl"), piMessages()...)
				return testSessionID
			},
			want: latestReplyWant{"latest reply", "2026-08-12T10:00:01Z", "read", "file body"},
		},
		{
			agent: "qoder",
			write: func(t *testing.T, home string) string {
				writeRows(t, filepath.Join(home, ".qoder", "projects", "-work", testSessionID+".jsonl"), claudeToolRows()...)
				return testSessionID
			},
			want: latestReplyWant{"latest reply", "2026-08-12T10:00:01Z", "Read", "file body"},
		},
	}
	for _, test := range tests {
		t.Run(test.agent, func(t *testing.T) {
			reader, home := testReader(t)
			sessionID := test.write(t, home)
			page, err := reader.ReadLatest(test.agent, ProjectContext{CWD: cwd}, sessionID, 80)
			assertLatestReply(t, page, err, test.want)
		})
	}
}

func claudeToolRows() []map[string]any {
	return []map[string]any{
		{"type": "user", "uuid": "u1", "timestamp": "2026-08-12T10:00:00Z", "message": map[string]any{"content": "question"}},
		{"type": "assistant", "uuid": "a1", "timestamp": "2026-08-12T10:00:01Z", "message": map[string]any{"content": []any{
			map[string]any{"type": "text", "text": "latest reply"},
			map[string]any{"type": "tool_use", "id": "tool-1", "name": "Read", "input": map[string]any{"path": "a.go"}},
		}}},
		{"type": "user", "uuid": "r1", "timestamp": "2026-08-12T10:00:02Z", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "tool-1", "content": "file body"},
		}}},
	}
}

func TestReadWithProjectLatestEntriesFollowClaudeChain(t *testing.T) {
	reader, home := testReader(t)
	child := "123e4567-e89b-12d3-a456-426614174001"
	root := filepath.Join(home, ".claude", "projects", "-work")
	writeRows(t, filepath.Join(root, testSessionID+".jsonl"),
		map[string]any{"type": "user", "uuid": "p1", "timestamp": "2026-08-12T09:00:00Z", "message": map[string]any{"content": "parent question"}},
		map[string]any{"type": "continued-in", "sessionId": testSessionID, "continuedInSessionId": child},
	)
	writeRows(t, filepath.Join(root, child+".jsonl"), claudeToolRows()...)

	page, err := reader.ReadLatest("claude", ProjectContext{CWD: "/work"}, testSessionID, 80)
	if err != nil || !page.Available || len(page.Entries) != 3 || page.Entries[0].Text != "parent question" {
		t.Fatalf("page = %#v, err = %v, want the parent prompt before the child turns", page, err)
	}
	assertLatestReply(t, page, err, latestReplyWant{"latest reply", "2026-08-12T10:00:01Z", "Read", "file body"})
}

func TestReadWithProjectLatestEntriesForOMO(t *testing.T) {
	home := t.TempDir()
	cwd := "/work/project"
	sessionID := "session-latest"
	directory := filepath.Join(home, ".omo", "agent", "sessions", encodeOMOCWD(cwd))
	writeRows(t, filepath.Join(directory, "2026-09-02T12-00-00-000Z_"+sessionID+".jsonl"),
		map[string]any{"type": "session", "id": sessionID, "cwd": cwd},
		map[string]any{"type": "message", "id": "u1", "timestamp": "2026-09-02T12:00:00Z", "message": map[string]any{"role": "user", "content": "question"}},
		map[string]any{"type": "message", "id": "a1", "timestamp": "2026-09-02T12:00:01Z", "message": map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "text", "text": "latest reply"},
			map[string]any{"type": "toolCall", "id": "call-1", "name": "read", "arguments": map[string]any{"path": "a.go"}},
		}}},
		map[string]any{"type": "message", "id": "t1", "timestamp": "2026-09-02T12:00:02Z", "message": map[string]any{"role": "toolResult", "toolCallId": "call-1", "content": "file body"}},
	)
	page, err := NewReader(home).ReadLatest("omo", ProjectContext{CWD: cwd}, sessionID, 80)
	assertLatestReply(t, page, err, latestReplyWant{"latest reply", "2026-09-02T12:00:01Z", "read", "file body"})
}

func TestReadWithProjectLatestEntriesForOpenCode(t *testing.T) {
	root := t.TempDir()
	assistant := `{"time":{"created":1792068926669},"agent":"build","content":[` +
		`{"type":"text","text":"latest reply"},` +
		`{"type":"tool","id":"call-1","name":"read","state":{"status":"completed","input":{"path":"a.go"},"content":[{"type":"text","text":"file body"}],"metadata":{"truncated":false}}}],"finish":"stop"}`
	sql := openCodeV2Schema + `INSERT INTO session_v2(id,directory,title,agent,time_updated) VALUES('ses_latest','/work','Latest','build',9);` +
		openCodeV2Message("m1", "ses_latest", "user", 1, 1792068900000, `{"time":{"created":1792068900000},"text":"question","files":[],"agents":[]}`) +
		openCodeV2Message("m2", "ses_latest", "assistant", 2, 1792068926669, assistant)
	createOpenCodeFixture(t, filepath.Join(root, "opencode.db"), sql)
	reader := openCodeFixtureReader(t, root)
	page, err := reader.ReadLatest("opencode", ProjectContext{CWD: "/work"}, "ses_latest", 80)
	assertLatestReply(t, page, err, latestReplyWant{"latest reply", "2026-10-15T12:55:26.669Z", "read", "file body"})
}

func TestReadWithProjectLatestEntriesForKimi(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	t.Setenv(agentroots.KimiCodeListEnv, root)
	t.Setenv("KIMI_CODE_HOME", "")
	cwd, id := "/fixture/kimi-project", "session_latest"
	wire := `{"type":"metadata","protocol_version":"1.5","created_at":1786680000000}` + "\n" +
		`{"type":"context.append_message","message":{"id":"u1","role":"user","content":[{"type":"text","text":"question"}],"toolCalls":[],"origin":{"kind":"user"}},"time":1786680001000}` + "\n" +
		`{"type":"context.append_message","message":{"id":"a1","role":"assistant","content":[{"type":"text","text":"latest reply"}],"toolCalls":[{"type":"function","id":"call-1","name":"read","arguments":"{\"path\":\"a.go\"}"}]},"time":1786680002000}` + "\n" +
		`{"type":"context.append_message","message":{"role":"tool","toolCallId":"call-1","content":[{"type":"text","text":"file body"}]},"time":1786680003000}` + "\n"
	kimiFixture(t, root, kimiWorkspaceKey(cwd), id, cwd, "Latest", 1, []byte(wire))
	page, err := NewReader(home).ReadLatest("kimi", ProjectContext{CWD: cwd}, id, 80)
	assertLatestReply(t, page, err, latestReplyWant{"latest reply", "2026-08-14T04:00:02Z", "read", "file body"})
}

func TestReadWithProjectLatestEntriesForCodex(t *testing.T) {
	reader, home := testReader(t)
	path := filepath.Join(home, ".codex", "sessions", "2026", "08", "12", "rollout-2026-08-12T10-00-00-"+testSessionID+".jsonl")
	writeRows(t, path,
		map[string]any{"timestamp": "2026-08-12T10:00:00Z", "type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "question"}}}},
		map[string]any{"timestamp": "2026-08-12T10:00:01Z", "type": "response_item", "payload": map[string]any{"type": "function_call", "call_id": "call-1", "name": "shell", "arguments": `{"cmd":"pwd"}`}},
		map[string]any{"timestamp": "2026-08-12T10:00:02Z", "type": "response_item", "payload": map[string]any{"type": "function_call_output", "call_id": "call-1", "output": "file body"}},
		map[string]any{"timestamp": "2026-08-12T10:00:03Z", "type": "response_item", "payload": map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "latest reply"}}}},
	)
	page, err := reader.ReadLatest("codex", ProjectContext{CWD: "/work"}, testSessionID, 80)
	if err != nil || !page.Available || len(page.Entries) != 3 {
		t.Fatalf("page = %#v, err = %v, want prompt, tool turn and reply", page, err)
	}
	call, reply := page.Entries[1], page.Entries[2]
	if call.Role != "assistant" || call.Timestamp != "2026-08-12T10:00:01Z" || len(call.Tools) != 1 ||
		call.Tools[0].Name != "shell" || call.Tools[0].Output != "file body" {
		t.Fatalf("tool entry = %#v", call)
	}
	if reply.Role != "assistant" || reply.Text != "latest reply" || reply.Timestamp != "2026-08-12T10:00:03Z" || len(reply.Tools) != 0 {
		t.Fatalf("reply entry = %#v", reply)
	}
}
