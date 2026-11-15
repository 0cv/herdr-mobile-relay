package app

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/activity"
	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
	"github.com/0cv/herdr-mobile-relay/internal/conversation"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
)

const (
	olderReply = "older finished reply"
	newerReply = "newer finished reply"
)

var (
	olderReplyAt = time.Date(2026, time.August, 12, 10, 0, 1, 0, time.UTC)
	newerReplyAt = olderReplyAt.Add(10 * time.Second)
)

func isolateProviderRoots(t *testing.T) {
	t.Helper()
	clearAgentEnv(t)
	for _, name := range []string{
		agentroots.CursorListEnv, agentroots.GrokListEnv, agentroots.OpenCodeListEnv, agentroots.OMOListEnv,
		agentroots.AgyListEnv, agentroots.KimiCodeListEnv, "GROK_HOME", "SENPI_CODING_AGENT_DIR",
		"OMO_CODING_AGENT_DIR", "KIMI_CODE_HOME",
	} {
		t.Setenv(name, "")
	}
}

func replyRFC3339(at time.Time) string { return at.Format(time.RFC3339Nano) }

type latestReplyCase struct {
	name        string
	agent       string
	timestamped bool
	write       func(t *testing.T, home string) (session, cwd string)
}

func latestReplyCases() []latestReplyCase {
	const anchor = "123e4567-e89b-12d3-a456-426614174000"
	const child = "123e4567-e89b-12d3-a456-426614174001"
	assistantBlocks := func(text string) []any {
		return []any{map[string]any{"type": "text", "text": text}}
	}
	piRows := func() []map[string]any {
		return []map[string]any{
			{"type": "message", "id": "u1", "timestamp": replyRFC3339(olderReplyAt.Add(-time.Second)), "message": map[string]any{"role": "user", "content": "first question"}},
			{"type": "message", "id": "a1", "timestamp": replyRFC3339(olderReplyAt), "message": map[string]any{"role": "assistant", "content": assistantBlocks(olderReply)}},
			{"type": "message", "id": "u2", "timestamp": replyRFC3339(newerReplyAt.Add(-time.Second)), "message": map[string]any{"role": "user", "content": "second question"}},
			{"type": "message", "id": "a2", "timestamp": replyRFC3339(newerReplyAt), "message": map[string]any{"role": "assistant", "content": assistantBlocks(newerReply)}},
		}
	}
	return []latestReplyCase{
		{name: "claude continuation", agent: "claude", timestamped: true, write: func(t *testing.T, home string) (string, string) {
			root := filepath.Join(home, ".claude", "projects", "-work-app")
			writeInvariantRows(t, filepath.Join(root, anchor+".jsonl"),
				map[string]any{"type": "user", "uuid": "u1", "timestamp": replyRFC3339(olderReplyAt.Add(-time.Second)), "message": map[string]any{"content": "first question"}},
				map[string]any{"type": "assistant", "uuid": "a1", "timestamp": replyRFC3339(olderReplyAt), "message": map[string]any{"content": assistantBlocks(olderReply)}},
				map[string]any{"type": "continued-in", "sessionId": anchor, "continuedInSessionId": child},
			)
			writeInvariantRows(t, filepath.Join(root, child+".jsonl"),
				map[string]any{"type": "user", "uuid": "u2", "timestamp": replyRFC3339(newerReplyAt.Add(-time.Second)), "message": map[string]any{"content": "second question"}},
				map[string]any{"type": "assistant", "uuid": "a2", "timestamp": replyRFC3339(newerReplyAt), "message": map[string]any{"content": assistantBlocks(newerReply)}},
			)
			return anchor, "/work/app"
		}},
		{name: "codex", agent: "codex", timestamped: true, write: func(t *testing.T, home string) (string, string) {
			message := func(role, kind, text string, at time.Time) map[string]any {
				return map[string]any{"timestamp": replyRFC3339(at), "type": "response_item", "payload": map[string]any{
					"type": "message", "role": role, "content": []any{map[string]any{"type": kind, "text": text}},
				}}
			}
			writeInvariantRows(t, filepath.Join(home, ".codex", "sessions", "2026", "08", "12", "rollout-2026-08-12T10-00-00-"+anchor+".jsonl"),
				message("user", "input_text", "first question", olderReplyAt.Add(-time.Second)),
				message("assistant", "output_text", olderReply, olderReplyAt),
				message("user", "input_text", "second question", newerReplyAt.Add(-time.Second)),
				message("assistant", "output_text", newerReply, newerReplyAt),
			)
			return anchor, "/work/app"
		}},
		{name: "pi", agent: "pi", timestamped: true, write: func(t *testing.T, home string) (string, string) {
			path := filepath.Join(home, ".pi", "agent", "sessions", "--work--", "session.jsonl")
			writeInvariantRows(t, path, piRows()...)
			return path, "/work/app"
		}},
		{name: "omp", agent: "omp", timestamped: true, write: func(t *testing.T, home string) (string, string) {
			writeInvariantRows(t, filepath.Join(home, ".omp", "agent", "sessions", "-work", "session_"+anchor+".jsonl"), piRows()...)
			return anchor, "/work/app"
		}},
		{name: "cursor", agent: "cursor", timestamped: false, write: func(t *testing.T, home string) (string, string) {
			cwd := filepath.Join(home, "work", "app")
			slug := cursorSlug(cwd)
			writeInvariantRows(t, filepath.Join(home, ".cursor", "projects", slug, "agent-transcripts", anchor, anchor+".jsonl"),
				map[string]any{"role": "user", "message": map[string]any{"content": "<user_query>first question</user_query>"}},
				map[string]any{"role": "assistant", "message": map[string]any{"content": olderReply}},
				map[string]any{"role": "user", "message": map[string]any{"content": "<user_query>second question</user_query>"}},
				map[string]any{"role": "assistant", "message": map[string]any{"content": newerReply}},
			)
			return anchor, cwd
		}},
		{name: "omo", agent: "omo", timestamped: true, write: func(t *testing.T, home string) (string, string) {
			const cwd, session = "/work/app", "session-latest"
			rows := append([]map[string]any{{"type": "session", "id": session, "cwd": cwd}}, piRows()...)
			writeInvariantRows(t, filepath.Join(home, ".omo", "agent", "sessions", "--work-app--", "2026-08-12T10-00-00-000Z_"+session+".jsonl"), rows...)
			return session, cwd
		}},
		{name: "grok", agent: "grok", timestamped: true, write: func(t *testing.T, home string) (string, string) {
			const cwd = "/work/app"
			var rows []map[string]any
			add := func(at time.Time, update map[string]any) {
				rows = append(rows, map[string]any{
					"timestamp": at.Unix(), "method": "session/update",
					"params": map[string]any{"sessionId": anchor, "update": update, "_meta": map[string]any{
						"eventId": fmt.Sprintf("event-%d", len(rows)), "agentTimestampMs": at.UnixMilli(),
					}},
				})
			}
			user := func(index int, text string, at time.Time) {
				add(at, map[string]any{
					"sessionUpdate": "user_message_chunk", "content": map[string]any{"type": "text", "text": text},
					"_meta": map[string]any{"modelId": "grok-test", "promptIndex": index},
				})
			}
			agent := func(text string, at time.Time) {
				add(at, map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": text}})
			}
			user(0, "first question", olderReplyAt.Add(-time.Second))
			agent(olderReply, olderReplyAt)
			user(1, "second question", newerReplyAt.Add(-time.Second))
			agent(newerReply, newerReplyAt)
			writeInvariantRows(t, filepath.Join(home, ".grok", "sessions", url.QueryEscape(cwd), anchor, "updates.jsonl"), rows...)
			return anchor, cwd
		}},
		{name: "hermes", agent: "hermes", timestamped: true, write: func(t *testing.T, home string) (string, string) {
			root, cwd, session := filepath.Join(home, "hermes"), t.TempDir(), "hermes-latest"
			t.Setenv(agentroots.HermesListEnv, root)
			database := openFixtureDatabase(t, filepath.Join(root, "state.db"))
			execFixtureSQL(t, database, `CREATE TABLE sessions(id TEXT PRIMARY KEY,cwd TEXT,title TEXT); CREATE TABLE messages(id INTEGER PRIMARY KEY,session_id TEXT,role TEXT,content TEXT,tool_call_id TEXT,tool_calls TEXT,tool_name TEXT,timestamp REAL,active INTEGER,compacted INTEGER,display_kind TEXT);`)
			execFixtureSQL(t, database, "INSERT INTO sessions VALUES(?,?,'Hermes')", session, cwd)
			for index, message := range []struct {
				role, text string
				at         time.Time
			}{
				{"user", "first question", olderReplyAt.Add(-time.Second)},
				{"assistant", olderReply, olderReplyAt},
				{"user", "second question", newerReplyAt.Add(-time.Second)},
				{"assistant", newerReply, newerReplyAt},
			} {
				execFixtureSQL(t, database, "INSERT INTO messages VALUES(?,?,?,?,NULL,NULL,NULL,?,1,0,'')",
					index+1, session, message.role, message.text, float64(message.at.UnixMilli())/1000)
			}
			return session, cwd
		}},
		{name: "opencode", agent: "opencode", timestamped: true, write: func(t *testing.T, home string) (string, string) {
			root, session := filepath.Join(home, "opencode"), "ses_latest"
			t.Setenv(agentroots.OpenCodeListEnv, root)
			database := openFixtureDatabase(t, filepath.Join(root, "opencode.db"))
			execFixtureSQL(t, database, `CREATE TABLE session_v2(id TEXT PRIMARY KEY,project_id TEXT NOT NULL DEFAULT 'p',directory TEXT NOT NULL,title TEXT,version TEXT NOT NULL DEFAULT '2',agent TEXT,time_created INTEGER NOT NULL DEFAULT 0,time_updated INTEGER NOT NULL DEFAULT 0); CREATE TABLE session_message(id TEXT PRIMARY KEY,session_id TEXT NOT NULL,type TEXT NOT NULL,seq INTEGER NOT NULL,time_created INTEGER NOT NULL,time_updated INTEGER NOT NULL DEFAULT 0,data TEXT NOT NULL); CREATE UNIQUE INDEX session_message_session_seq_idx ON session_message(session_id,seq);`)
			execFixtureSQL(t, database, "INSERT INTO session_v2(id,directory,title,agent,time_updated) VALUES(?,'/work/app','OpenCode','build',9)", session)
			for index, message := range []struct {
				kind, data string
				at         time.Time
			}{
				{"user", `{"text":"first question","files":[],"agents":[]}`, olderReplyAt.Add(-time.Second)},
				{"assistant", fmt.Sprintf(`{"agent":"build","content":[{"type":"text","text":%q}],"finish":"stop"}`, olderReply), olderReplyAt},
				{"user", `{"text":"second question","files":[],"agents":[]}`, newerReplyAt.Add(-time.Second)},
				{"assistant", fmt.Sprintf(`{"agent":"build","content":[{"type":"text","text":%q}],"finish":"stop"}`, newerReply), newerReplyAt},
			} {
				execFixtureSQL(t, database, "INSERT INTO session_message(id,session_id,type,seq,time_created,data) VALUES(?,?,?,?,?,?)",
					fmt.Sprintf("m%d", index+1), session, message.kind, index+1, message.at.UnixMilli(), message.data)
			}
			return session, "/work/app"
		}},
		{name: "antigravity", agent: "agy", timestamped: true, write: func(t *testing.T, home string) (string, string) {
			const session = "071dc833-ce1a-4df3-b198-03af792b4ae0"
			root := filepath.Join(home, "agy")
			t.Setenv(agentroots.AgyListEnv, root)
			database := openFixtureDatabase(t, filepath.Join(root, "conversations", session+".db"))
			execFixtureSQL(t, database, "CREATE TABLE `steps` (`idx` integer,`step_type` integer NOT NULL DEFAULT 0,`status` integer NOT NULL DEFAULT 0,`metadata` blob,`step_payload` blob,PRIMARY KEY (`idx`))")
			for index, step := range []struct {
				kind   uint64
				at     time.Time
				text   string
				isUser bool
			}{
				{14, olderReplyAt.Add(-time.Second), "first question", true},
				{15, olderReplyAt, olderReply, false},
				{14, newerReplyAt.Add(-time.Second), "second question", true},
				{15, newerReplyAt, newerReply, false},
			} {
				execFixtureSQL(t, database, "INSERT INTO steps(idx,step_type,status,metadata,step_payload) VALUES(?,?,3,?,?)",
					index, step.kind, []byte{}, agyStepPayload(step.kind, step.at, step.text, step.isUser))
			}
			return session, "/work/app"
		}},
	}
}

func cursorSlug(cwd string) string {
	slug := make([]byte, 0, len(cwd))
	for _, character := range []byte(cwd) {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' {
			slug = append(slug, character)
			continue
		}
		slug = append(slug, '-')
	}
	for len(slug) > 0 && slug[0] == '-' {
		slug = slug[1:]
	}
	return string(slug)
}

func agyField(number int, value uint64) []byte {
	return binary.AppendUvarint(binary.AppendUvarint(nil, uint64(number)<<3), value)
}

func agyBytes(number int, value []byte) []byte {
	out := binary.AppendUvarint(nil, uint64(number)<<3|2)
	out = binary.AppendUvarint(out, uint64(len(value)))
	return append(out, value...)
}

func agyStepPayload(kind uint64, at time.Time, text string, user bool) []byte {
	stamp := agyBytes(1, append(agyField(1, uint64(at.Unix())), agyField(2, 500)...))
	payload := append(agyField(1, kind), agyBytes(5, stamp)...)
	if user {
		return append(payload, agyBytes(19, agyBytes(2, []byte(text)))...)
	}
	return append(payload, agyBytes(20, agyBytes(1, []byte(text)))...)
}

func openFixtureDatabase(t *testing.T, path string) *sql.DB {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	database.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func execFixtureSQL(t *testing.T, database *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := database.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func latestReplyServer(t *testing.T, test latestReplyCase) (*Server, *coordinator.AgentState) {
	t.Helper()
	isolateProviderRoots(t)
	home := t.TempDir()
	session, cwd := test.write(t, home)
	server := testServer()
	server.conversationM = conversation.NewReader(home)
	server.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", Agent: test.agent, Cwd: cwd, SessionID: session, Status: "idle",
	}}, server.state.RevisionCounter())
	agent, _ := server.state.Agent("pane-1")
	return server, agent
}

func TestLatestConversationResponseReturnsNewestReplyPerProvider(t *testing.T) {
	for _, test := range latestReplyCases() {
		t.Run(test.name, func(t *testing.T) {
			server, agent := latestReplyServer(t, test)
			if got := server.latestConversationResponse(agent.Agent, agent.SessionID, projectContextForAgent(agent)); got != newerReply {
				t.Fatalf("latest response = %q, want %q", got, newerReply)
			}
		})
	}
}

func TestEnrichActivityResponsesReturnsNewestReplyAtOrBeforeActivityPerProvider(t *testing.T) {
	for _, test := range latestReplyCases() {
		t.Run(test.name, func(t *testing.T) {
			server, agent := latestReplyServer(t, test)
			extract := func(at time.Time) string {
				entries := server.enrichActivityResponses([]activity.Entry{{
					Kind: "finished", PaneID: agent.PaneID, Agent: agent.Agent, Session: agent.SessionID,
					Timestamp: activity.MilliTimestamp(at.UnixMilli()),
				}})
				return entries[0].Extract
			}
			untimed := server.enrichActivityResponses([]activity.Entry{{
				Kind: "finished", PaneID: agent.PaneID, Agent: agent.Agent, Session: agent.SessionID,
			}})
			if untimed[0].Extract != newerReply {
				t.Fatalf("activity without a time = %q, want the newest reply %q", untimed[0].Extract, newerReply)
			}
			want := func(reply string) string {
				if !test.timestamped {
					return ""
				}
				return reply
			}
			if got := extract(newerReplyAt.Add(time.Minute)); got != want(newerReply) {
				t.Fatalf("activity after both replies = %q, want %q", got, want(newerReply))
			}
			if got := extract(newerReplyAt); got != want(newerReply) {
				t.Fatalf("activity at the newest reply = %q, want %q", got, want(newerReply))
			}
			if got := extract(olderReplyAt.Add(5 * time.Second)); got != want(olderReply) {
				t.Fatalf("activity between the replies = %q, want %q", got, want(olderReply))
			}
			if got := extract(olderReplyAt.Add(-time.Minute)); got != "" {
				t.Fatalf("activity before both replies = %q, want none", got)
			}
		})
	}
}
