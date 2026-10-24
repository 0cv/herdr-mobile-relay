package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
	"github.com/0cv/herdr-mobile-relay/internal/conversation"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
)

func TestDatabaseRecordedConversation(t *testing.T) {
	for _, format := range []string{"v1", "v2", "mixed-v1", "mixed-v2", "hermes"} {
		t.Run(format, func(t *testing.T) {
			testDatabaseRecordedConversation(t, format, false)
		})
	}
}

func TestDatabaseRecordedConversationEntrypoint(t *testing.T) {
	for _, format := range []string{"v1", "v2", "mixed-v1", "mixed-v2", "hermes"} {
		t.Run(format, func(t *testing.T) {
			testDatabaseRecordedConversation(t, format, true)
		})
	}
}

func testDatabaseRecordedConversation(t *testing.T, format string, entrypoint bool) {
	root := t.TempDir()
	t.Setenv(agentroots.OpenCodeListEnv, root)
	t.Setenv(agentroots.HermesListEnv, root)
	t.Setenv("HERMES_HOME", "")
	provider, session, name := "opencode", "ses_live123", "opencode.db"
	if format == "hermes" {
		provider, session, name = "Hermes-Agent", "hermes-live", "state.db"
	}
	if !supportsConversationActivity(provider) {
		t.Fatal("database provider excluded from recorded feed")
	}
	database := filepath.Join(root, name)
	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("PRAGMA journal_mode=WAL")
	v2 := format == "v2" || format == "mixed-v2"
	if format == "hermes" {
		exec(`CREATE TABLE sessions(id TEXT PRIMARY KEY,cwd TEXT,title TEXT); CREATE TABLE messages(id INTEGER PRIMARY KEY,session_id TEXT,role TEXT,content TEXT,tool_call_id TEXT,tool_calls TEXT,tool_name TEXT,timestamp REAL,active INTEGER,compacted INTEGER,display_kind TEXT);`)
		exec("INSERT INTO sessions VALUES(?,?,'Live')", session, root)
	} else {
		if format != "v2" {
			exec(`CREATE TABLE session(id TEXT PRIMARY KEY,directory TEXT,title TEXT,time_updated INTEGER,agent TEXT); CREATE TABLE message(id TEXT PRIMARY KEY,session_id TEXT,time_created INTEGER,data TEXT); CREATE INDEX message_order ON message(session_id,time_created,id); CREATE TABLE part(id TEXT PRIMARY KEY,message_id TEXT,data TEXT); CREATE INDEX part_message ON part(message_id);`)
			exec("INSERT INTO session VALUES(?,?,'Live',1,'opencode')", session, root)
		}
		if format != "v1" {
			exec(`CREATE TABLE session_v2(id TEXT PRIMARY KEY,directory TEXT,title TEXT,time_updated INTEGER,agent TEXT); CREATE TABLE session_message(id TEXT PRIMARY KEY,session_id TEXT,type TEXT,seq INTEGER,time_created INTEGER,data TEXT); CREATE INDEX session_order ON session_message(session_id,seq);`)
			exec("INSERT INTO session_v2 VALUES(?,?,'Live',1,'opencode')", session, root)
		}
	}
	appendMessage := func(index int, text string) {
		t.Helper()
		id := fmt.Sprintf("m%04d", index)
		if format == "hermes" {
			exec("INSERT INTO messages VALUES(?,?,'assistant',?,NULL,NULL,NULL,?,1,0,'')", index, session, text, index)
		} else if v2 {
			raw, _ := json.Marshal(map[string]any{"content": []map[string]any{{"type": "text", "text": text}}})
			exec("INSERT INTO session_message VALUES(?,?,'assistant',?,?,?)", id, session, index, index, string(raw))
		} else {
			exec("INSERT INTO message VALUES(?,?,?,'{\"role\":\"assistant\"}')", id, session, index)
			raw, _ := json.Marshal(map[string]any{"type": "text", "text": text})
			exec("INSERT INTO part VALUES(?,?,?)", "p"+id, id, string(raw))
		}
	}
	for i := 1; i <= 220; i++ {
		appendMessage(i, fmt.Sprintf("seed %d", i))
	}
	server := testServerWithCacheDir(t.TempDir())
	server.conversationB.Close()
	reader := conversation.NewReader(t.TempDir())
	browser, err := conversation.NewBrowser(reader, t.TempDir(), conversation.DefaultBrowserOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	defer server.herdrC.Close()
	server.conversationM, server.conversationB = reader, browser
	server.state.CommitInventory([]*coordinator.AgentState{{PaneID: "db-pane", TerminalID: "db-terminal", ServerSessionID: "primary", Agent: provider, Cwd: root, SessionID: session, AgentSessionID: session, Status: "working"}}, server.state.RevisionCounter())
	agent, _ := server.state.Agent("db-pane")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	feed := &conversationFeed{ctx: ctx, cancel: cancel, target: protocol.TargetRef{PaneID: agent.PaneID, TerminalID: agent.TerminalID, ServerSessionID: agent.ServerSessionID, AgentSessionID: session, Generation: agent.Generation}, agent: agent, ready: make(chan struct{}), notify: make(chan struct{})}
	watch := browser.NewDatabaseWatch(conversationScope(agent))
	observed := &databaseRefreshObserver{watch: watch, feed: feed}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if entrypoint {
			server.runRecordedConversationFeed(feed)
			return
		}
		server.runDatabaseConversationFeed(feed, observed)
	}()
	defer func() { cancel(); <-done }()
	var seq uint64
	wait := func(kind, text string) conversationBatch {
		t.Helper()
		batch := waitRecordedDatabaseBatch(t, feed, &seq, time.After(5*time.Second))
		if batch.Kind != kind {
			t.Fatalf("kind %s, want %s: %+v", batch.Kind, kind, batch)
		}
		if text != "" && (len(batch.Entries) != 1 || batch.Entries[0].Text != text) {
			t.Fatalf("changed records = %+v, want %s", batch.Entries, text)
		}
		return batch
	}
	initial := wait("batch", "")
	if entrypoint {
		feed.mu.Lock()
		wired := feed.database != nil
		feed.mu.Unlock()
		if !wired {
			t.Fatal("recorded entrypoint did not install the real database watch")
		}
	}
	if len(initial.Entries) != 200 || initial.Capabilities.Source != "recorded_history" {
		t.Fatalf("unbounded or wrong source: %d", len(initial.Entries))
	}
	for _, path := range []string{database, database + "-wal"} {
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := browser.ReadPage(ctx, conversation.BrowseRequest{Scope: conversationScope(agent), Limit: 200}); err != nil {
			t.Fatal(err)
		}
		after, err := os.ReadFile(path)
		if err != nil || sha256.Sum256(before) != sha256.Sum256(after) {
			t.Fatal("observation modified database or WAL")
		}
	}
	appendMessage(221, "appended")
	wait("batch", "appended")
	if entrypoint {
		return
	}
	if format == "hermes" {
		exec("UPDATE messages SET content='edited' WHERE id=221")
	} else if v2 {
		exec(`UPDATE session_message SET data='{"content":[{"type":"text","text":"edited"}]}' WHERE id='m0221'`)
	} else {
		exec(`UPDATE part SET data='{"type":"text","text":"edited"}' WHERE id='pm0221'`)
	}
	wait("batch", "edited")
	if format == "hermes" {
		exec(`UPDATE messages SET tool_calls='[{"id":"call","function":{"name":"Read","arguments":"{}"}}]' WHERE id=221`)
	} else if v2 {
		exec(`UPDATE session_message SET data='{"content":[{"type":"text","text":"edited"},{"type":"tool","id":"call","name":"Read","state":{"status":"running","input":{}}}]}' WHERE id='m0221'`)
	} else {
		exec(`INSERT INTO part VALUES('tool','m0221','{"type":"tool","callID":"call","tool":"Read","state":{"status":"running","input":{}}}')`)
	}
	running := wait("batch", "edited")
	if len(running.Entries[0].Tools) != 1 || running.Entries[0].Tools[0].State != "running" {
		t.Fatalf("running tool missing: %+v", running)
	}
	if format == "hermes" {
		exec("INSERT INTO messages VALUES(222,?,'tool','done','call',NULL,'Read',222,1,0,'')", session)
	} else if v2 {
		exec(`UPDATE session_message SET data='{"content":[{"type":"text","text":"edited"},{"type":"tool","id":"call","name":"Read","state":{"status":"completed","input":{},"output":"done"}}]}' WHERE id='m0221'`)
	} else {
		exec(`UPDATE part SET data='{"type":"tool","callID":"call","tool":"Read","state":{"status":"completed","input":{},"output":"done"}}' WHERE id='tool'`)
	}
	completed := wait("batch", "edited")
	if completed.Entries[0].Tools[0].State != "completed" || completed.Entries[0].Tools[0].Output != "done" {
		t.Fatalf("completed tool missing: %+v", completed)
	}
	exec("VACUUM")
	vacuum := wait("reset", "")
	if vacuum.SourceRevision == completed.SourceRevision {
		t.Fatal("vacuum retained source generation")
	}
	exec("PRAGMA wal_checkpoint(TRUNCATE)")
	wait("reset", "")
	appendMessage(223, "after checkpoint")
	wait("reset", "")
	if format == "hermes" {
		exec("DELETE FROM messages WHERE id=223")
	} else if v2 {
		exec("DELETE FROM session_message WHERE id='m0223'")
	} else {
		exec("DELETE FROM message WHERE id='m0223'")
	}
	wait("reset", "")
	exec("PRAGMA wal_checkpoint(TRUNCATE)")
	_ = db.Close()
	wait("reset", "")
	raw, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(database+".replacement", raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(database+".replacement", database); err != nil {
		t.Fatal(err)
	}
	wait("reset", "")
	if err := os.Remove(database); err != nil {
		t.Fatal(err)
	}
	waitDatabaseUnavailable(t, feed, observed, &seq)
	seqBefore := seq
	time.Sleep(550 * time.Millisecond)
	feed.mu.Lock()
	if feed.sequence != seqBefore {
		t.Error("unavailable source repeats resets")
	}
	feed.mu.Unlock()
	server.state.CommitInventory([]*coordinator.AgentState{{PaneID: agent.PaneID, TerminalID: "replacement", Agent: provider, SessionID: session}}, server.state.RevisionCounter())
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stale target continued polling")
	}
}
