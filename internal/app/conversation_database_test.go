package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
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
			testDatabaseRecordedConversation(t, format, false, false)
		})
	}
}

func TestDatabaseRecordedConversationEntrypoint(t *testing.T) {
	for _, format := range []string{"v1", "v2", "mixed-v1", "mixed-v2", "hermes"} {
		t.Run(format, func(t *testing.T) {
			testDatabaseRecordedConversation(t, format, true, false)
		})
	}
}

// The first write after a WAL truncation puts a new WAL header on disk before
// its commit is published, so a refresh in between resets without the message.
func TestDatabaseRecordedConversationRestartPublishedAfterRefresh(t *testing.T) {
	for _, format := range []string{"v1", "v2", "hermes"} {
		t.Run(format, func(t *testing.T) {
			testDatabaseRecordedConversation(t, format, false, true)
		})
	}
}

func testDatabaseRecordedConversation(t *testing.T, format string, entrypoint, lateRestart bool) {
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
	insertMessage := func(index int, text string) error {
		id := fmt.Sprintf("m%04d", index)
		if format == "hermes" {
			_, err := db.Exec("INSERT INTO messages VALUES(?,?,'assistant',?,NULL,NULL,NULL,?,1,0,'')", index, session, text, index)
			return err
		}
		if v2 {
			raw, _ := json.Marshal(map[string]any{"content": []map[string]any{{"type": "text", "text": text}}})
			_, err := db.Exec("INSERT INTO session_message VALUES(?,?,'assistant',?,?,?)", id, session, index, index, string(raw))
			return err
		}
		if _, err := db.Exec("INSERT INTO message VALUES(?,?,?,'{\"role\":\"assistant\"}')", id, session, index); err != nil {
			return err
		}
		raw, _ := json.Marshal(map[string]any{"type": "text", "text": text})
		_, err := db.Exec("INSERT INTO part VALUES(?,?,?)", "p"+id, id, string(raw))
		return err
	}
	appendMessage := func(index int, text string) {
		t.Helper()
		if err := insertMessage(index, text); err != nil {
			t.Fatal(err)
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
	late := &heldPublicationRefresh{databaseConversationRefresh: watch}
	observed := &databaseRefreshObserver{watch: late, feed: feed}
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
	if lateRestart {
		held := make(chan struct{})
		late.mu.Lock()
		late.hold = func() (func() error, error) {
			defer close(held)
			return holdWALPublication(database, func() error { return insertMessage(223, "after checkpoint") })
		}
		late.mu.Unlock()
		select {
		case <-held:
		case <-time.After(3 * time.Second):
			t.Fatal("feed stopped polling")
		}
	} else {
		appendMessage(223, "after checkpoint")
	}
	restart := wait("reset", "")
	if err := late.failure(); err != nil {
		t.Fatal(err)
	}
	refreshed, err := observed.result(restart)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(refreshed.texts, "after checkpoint") {
		wait("batch", "after checkpoint")
	}
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

// heldPublicationRefresh lets the feed sample its change token while a commit
// is in the WAL but not yet published to readers, then publishes the commit
// after the refresh that read the older snapshot.
type heldPublicationRefresh struct {
	databaseConversationRefresh
	mu      sync.Mutex
	hold    func() (func() error, error)
	publish func() error
	err     error
}

func (r *heldPublicationRefresh) NeedsReset() bool {
	r.mu.Lock()
	if r.hold != nil {
		r.publish, r.err = r.hold()
		r.hold = nil
	}
	r.mu.Unlock()
	return r.databaseConversationRefresh.NeedsReset()
}

func (r *heldPublicationRefresh) Refresh(ctx context.Context) (conversation.BrowsePage, bool) {
	page, reset := r.databaseConversationRefresh.Refresh(ctx)
	r.mu.Lock()
	if r.publish != nil && r.err == nil {
		r.err = r.publish()
		r.publish = nil
	}
	r.mu.Unlock()
	return page, reset
}

func (r *heldPublicationRefresh) failure() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// holdWALPublication runs write, then puts back SQLite's WAL-index header so
// readers still see the previous commit, as they do while a writer is between
// appending its WAL frames and publishing them. The returned function publishes.
func holdWALPublication(database string, write func() error) (func() error, error) {
	shm, err := os.OpenFile(database+"-shm", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	var before, after [96]byte
	if _, err := shm.ReadAt(before[:], 0); err != nil {
		return nil, errors.Join(err, shm.Close())
	}
	if err := write(); err != nil {
		return nil, errors.Join(err, shm.Close())
	}
	if _, err := shm.ReadAt(after[:], 0); err != nil {
		return nil, errors.Join(err, shm.Close())
	}
	if before == after {
		return nil, errors.Join(errors.New("write did not publish a WAL commit"), shm.Close())
	}
	if _, err := shm.WriteAt(before[:], 0); err != nil {
		return nil, errors.Join(err, shm.Close())
	}
	return func() error {
		_, err := shm.WriteAt(after[:], 0)
		return errors.Join(err, shm.Close())
	}, nil
}

func TestDatabaseFeedRefreshesCommitPublishedAfterItsWALWrite(t *testing.T) {
	root := t.TempDir()
	t.Setenv(agentroots.OpenCodeListEnv, root)
	t.Setenv(agentroots.HermesListEnv, root)
	t.Setenv("HERMES_HOME", "")
	session := "ses_live123"
	database := filepath.Join(root, "opencode.db")
	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	for _, query := range []string{
		"PRAGMA journal_mode=WAL",
		`CREATE TABLE session_v2(id TEXT PRIMARY KEY,directory TEXT,title TEXT,time_updated INTEGER,agent TEXT); CREATE TABLE session_message(id TEXT PRIMARY KEY,session_id TEXT,type TEXT,seq INTEGER,time_created INTEGER,data TEXT); CREATE INDEX session_order ON session_message(session_id,seq);`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec("INSERT INTO session_v2 VALUES(?,?,'Live',1,'opencode')", session, root); err != nil {
		t.Fatal(err)
	}
	appendMessage := func(index int, text string) error {
		raw, _ := json.Marshal(map[string]any{"content": []map[string]any{{"type": "text", "text": text}}})
		_, err := db.Exec("INSERT INTO session_message VALUES(?,?,'assistant',?,?,?)", fmt.Sprintf("m%04d", index), session, index, index, string(raw))
		return err
	}
	if err := appendMessage(1, "seed"); err != nil {
		t.Fatal(err)
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
	server.state.CommitInventory([]*coordinator.AgentState{{PaneID: "db-pane", TerminalID: "db-terminal", ServerSessionID: "primary", Agent: "opencode", Cwd: root, SessionID: session, AgentSessionID: session, Status: "working"}}, server.state.RevisionCounter())
	agent, _ := server.state.Agent("db-pane")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	feed := &conversationFeed{ctx: ctx, cancel: cancel, target: protocol.TargetRef{PaneID: agent.PaneID, TerminalID: agent.TerminalID, ServerSessionID: agent.ServerSessionID, AgentSessionID: session, Generation: agent.Generation}, agent: agent, ready: make(chan struct{}), notify: make(chan struct{})}
	watch := &heldPublicationRefresh{databaseConversationRefresh: browser.NewDatabaseWatch(conversationScope(agent))}
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.runDatabaseConversationFeed(feed, watch)
	}()
	defer func() { cancel(); <-done }()
	var seq uint64
	if initial := waitRecordedDatabaseBatch(t, feed, &seq, time.After(5*time.Second)); initial.Kind != "batch" || len(initial.Entries) != 1 {
		t.Fatalf("initial page = %+v", initial)
	}
	held := make(chan struct{})
	watch.mu.Lock()
	watch.hold = func() (func() error, error) {
		defer close(held)
		return holdWALPublication(database, func() error { return appendMessage(2, "appended") })
	}
	watch.mu.Unlock()
	select {
	case <-held:
	case <-time.After(3 * time.Second):
		t.Fatal("feed stopped polling")
	}
	if err := watch.failure(); err != nil {
		t.Fatal(err)
	}
	batch := waitRecordedDatabaseBatch(t, feed, &seq, time.After(3*time.Second))
	if batch.Kind != "batch" || len(batch.Entries) != 1 || batch.Entries[0].Text != "appended" {
		t.Fatalf("published commit = %+v", batch)
	}
}
