package conversation

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
	"github.com/0cv/herdr-mobile-relay/internal/sqliteexec"
)

func openCodePendingFixture(t *testing.T, format string) (*sql.DB, *Browser, *DatabaseWatch, *liveBudgetExecutor) {
	t.Helper()
	root := t.TempDir()
	t.Setenv(agentroots.OpenCodeListEnv, root)
	db, err := sql.Open("sqlite", filepath.Join(root, "opencode.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	liveExec(t, db, "PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0")
	if format == "v1" {
		liveExec(t, db, `CREATE TABLE session(id TEXT PRIMARY KEY,directory TEXT,title TEXT,time_updated INTEGER,agent TEXT);
CREATE TABLE message(id TEXT PRIMARY KEY,session_id TEXT NOT NULL,time_created INTEGER NOT NULL,time_updated INTEGER NOT NULL,data TEXT NOT NULL);
CREATE TABLE part(id TEXT PRIMARY KEY,message_id TEXT NOT NULL,session_id TEXT NOT NULL,time_created INTEGER NOT NULL,time_updated INTEGER NOT NULL,data TEXT NOT NULL);
CREATE INDEX message_session_time_created_id_idx ON message(session_id,time_created,id);
CREATE INDEX part_message_id_id_idx ON part(message_id,id);
INSERT INTO session VALUES('ses_pending','/fixture','Pending',1,'opencode');`)
	} else {
		liveExec(t, db, `CREATE TABLE session_v2(id TEXT PRIMARY KEY,directory TEXT,title TEXT,time_updated INTEGER,agent TEXT);
CREATE TABLE session_message(id TEXT PRIMARY KEY,session_id TEXT NOT NULL,type TEXT NOT NULL,seq INTEGER NOT NULL,time_created INTEGER NOT NULL,time_updated INTEGER NOT NULL,data TEXT NOT NULL);
CREATE UNIQUE INDEX session_message_session_seq_idx ON session_message(session_id,seq);
CREATE INDEX session_message_session_type_seq_idx ON session_message(session_id,type,seq);
CREATE INDEX session_message_session_time_created_id_idx ON session_message(session_id,time_created,id);
INSERT INTO session_v2 VALUES('ses_pending','/fixture','Pending',1,'opencode');`)
	}
	reader := NewReader(t.TempDir())
	executor := &liveBudgetExecutor{t: t, base: sqliteexec.MustFromEnv(), plans: make(map[string]bool)}
	reader.openCode.executor = executor
	browser, err := NewBrowser(reader, t.TempDir(), DefaultBrowserOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = browser.Close() })
	watch := browser.NewDatabaseWatch(BrowseScope{Provider: "OpenCode", SessionID: "ses_pending"})
	return db, browser, watch, executor
}

func insertOpenCodePending(t *testing.T, db *sql.DB, format, id, role string, seq int) {
	t.Helper()
	if format == "v1" {
		data, _ := json.Marshal(map[string]string{"role": role, "provider": "MUST_NOT_PUBLISH_METADATA"})
		liveExec(t, db, `INSERT INTO message VALUES(?,'ses_pending',?,?,?)`, id, seq, seq, string(data))
		return
	}
	liveExec(t, db, `INSERT INTO session_message VALUES(?,'ses_pending',?,?,?,?,'{"content":[],"metadata":"MUST_NOT_PUBLISH_METADATA"}')`, id, role, seq, seq, seq)
}

func saveOpenCodeBody(t *testing.T, db *sql.DB, format, id, text string) {
	t.Helper()
	if format == "v1" {
		data, _ := json.Marshal(map[string]string{"type": "text", "text": text})
		liveExec(t, db, `INSERT INTO part VALUES(?,?,'ses_pending',1,1,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data,time_updated=time_updated+1`, "p"+id, id, string(data))
		return
	}
	data, _ := json.Marshal(map[string]any{"content": []map[string]string{{"type": "reasoning", "text": "MUST_NOT_PUBLISH_REASONING"}, {"type": "text", "text": text}}, "metadata": "MUST_NOT_PUBLISH_METADATA"})
	liveExec(t, db, `UPDATE session_message SET data=?,time_updated=time_updated+1 WHERE id=?`, string(data), id)
}

func TestOpenCodePendingSavedBody(t *testing.T) {
	for _, format := range []string{"v1", "v2"} {
		for _, populated := range []bool{false, true} {
			for _, initial := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/populated=%v/initial=%v", format, populated, initial), func(t *testing.T) {
					db, _, watch, executor := openCodePendingFixture(t, format)
					if populated {
						insertOpenCodePending(t, db, format, "user", "user", 1)
						if format == "v2" {
							liveExec(t, db, `UPDATE session_message SET data='{"text":"exact prompt"}' WHERE id='user'`)
						} else {
							saveOpenCodeBody(t, db, format, "user", "exact prompt")
						}
					}
					if initial {
						insertOpenCodePending(t, db, format, "answer", "assistant", 2)
					}
					page, reset := watch.Refresh(t.Context())
					if reset || page.State != BrowseReady || len(page.Entries) != map[bool]int{false: 0, true: 1}[populated] {
						t.Fatalf("snapshot: reset=%v page=%+v", reset, page)
					}
					if !initial {
						insertOpenCodePending(t, db, format, "answer", "assistant", 2)
					}
					executor.active = true
					for _, text := range []string{"", "saved partial", "exact final answer"} {
						page, reset = watch.Refresh(t.Context())
						if reset || len(page.Entries) != 0 {
							t.Fatalf("unchanged pending: reset=%v page=%+v", reset, page)
						}
						saveOpenCodeBody(t, db, format, "answer", text)
						page, reset = watch.Refresh(t.Context())
						if reset || page.State != BrowseReady {
							t.Fatalf("saved body: reset=%v page=%+v", reset, page)
						}
						if text == "" {
							if len(page.Entries) != 0 {
								t.Fatalf("empty body exposed: %+v", page)
							}
							continue
						}
						if len(page.Entries) != 1 || page.Entries[0].ID != "answer" || page.Entries[0].Text != text || page.RecordedEdits["answer"] != (text == "exact final answer") {
							t.Fatalf("body missing or incorrectly classified: %+v", page)
						}
						raw, _ := json.Marshal(page)
						if bytes.Contains(raw, []byte("MUST_NOT_PUBLISH")) {
							t.Fatal("private native fields exposed")
						}
					}
					database := filepath.Join(filepath.Dir(watch.path), "opencode.db")
					for _, path := range []string{database, database + "-wal"} {
						before, err := os.ReadFile(path)
						if err != nil {
							t.Fatal(err)
						}
						page, reset = watch.Refresh(t.Context())
						after, err := os.ReadFile(path)
						if err != nil || !bytes.Equal(before, after) || reset || len(page.Entries) != 0 {
							t.Fatal("read modified source or repeated saved content")
						}
					}
				})
			}
		}
	}
}

type openCodeSnapshotExecutor struct {
	sqliteexec.Executor
	after func()
}

func (e *openCodeSnapshotExecutor) QueryJSON(ctx context.Context, database, query string, maxBytes int) ([]byte, error) {
	raw, err := e.Executor.QueryJSON(ctx, database, query, maxBytes)
	if err == nil && e.after != nil && strings.Contains(query, "LEFT JOIN selected AS sm") {
		after := e.after
		e.after = nil
		after()
	}
	return raw, err
}

func TestOpenCodePendingDuringSnapshot(t *testing.T) {
	for _, format := range []string{"v1", "v2"} {
		t.Run(format, func(t *testing.T) {
			db, _, watch, executor := openCodePendingFixture(t, format)
			insertOpenCodePending(t, db, format, "answer", "assistant", 1)
			executor.base = &openCodeSnapshotExecutor{Executor: executor.base, after: func() {
				saveOpenCodeBody(t, db, format, "answer", "saved during snapshot")
			}}
			page, reset := watch.Refresh(t.Context())
			if reset || len(page.Entries) != 0 {
				t.Fatalf("snapshot: reset=%v page=%+v", reset, page)
			}
			raw, _ := json.Marshal(page)
			if bytes.Contains(raw, []byte("answer")) {
				t.Fatal("pending ID exposed on wire")
			}
			executor.active = true
			page, reset = watch.Refresh(t.Context())
			if reset || len(page.Entries) != 1 || page.Entries[0].Text != "saved during snapshot" || page.RecordedEdits["answer"] {
				t.Fatalf("snapshot race lost body: reset=%v page=%+v", reset, page)
			}
		})
	}
}

func TestOpenCodePendingRemovalAndBounds(t *testing.T) {
	for _, format := range []string{"v1", "v2"} {
		t.Run(format, func(t *testing.T) {
			db, _, watch, executor := openCodePendingFixture(t, format)
			insertOpenCodePending(t, db, format, "empty", "assistant", 1)
			insertOpenCodePending(t, db, format, "answer", "assistant", 2)
			page, reset := watch.Refresh(t.Context())
			if reset || page.State != BrowseReady {
				t.Fatalf("snapshot: reset=%v page=%+v", reset, page)
			}
			executor.active = true
			liveExec(t, db, "DELETE FROM "+watch.table()+" WHERE id='empty'")
			page, reset = watch.Refresh(t.Context())
			if reset || len(page.Entries) != 0 || len(watch.loaded) != 1 {
				t.Fatalf("removed placeholder retained: reset=%v loaded=%v page=%+v", reset, watch.loaded, page)
			}
			saveOpenCodeBody(t, db, format, "answer", "visible")
			page, reset = watch.Refresh(t.Context())
			if reset || len(page.Entries) != 1 {
				t.Fatalf("visible body: reset=%v page=%+v", reset, page)
			}
			saveOpenCodeBody(t, db, format, "answer", "")
			if _, reset = watch.Refresh(t.Context()); !reset {
				t.Fatal("removing previously visible text must reset")
			}
			page = BrowsePage{Available: true, State: BrowseReady, SourceRevision: watch.revision}
			for i := 0; i <= databaseLiveLoaded; i++ {
				page.recordedPending = append(page.recordedPending, fmt.Sprintf("pending-%d", i))
			}
			queries := executor.queries
			if !watch.Observe(page, true) || len(watch.loaded) != databaseLiveLoaded || executor.queries != queries {
				t.Fatal("pending IDs escaped loaded-ID or no-SQL observation budget")
			}
		})
	}
}
