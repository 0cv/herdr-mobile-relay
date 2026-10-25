package conversation

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
	"github.com/0cv/herdr-mobile-relay/internal/sqliteexec"
)

type changeCountingExecutor struct {
	sqliteexec.Executor
	queries int
}

func (e *changeCountingExecutor) QueryJSON(ctx context.Context, database, query string, maxBytes int) ([]byte, error) {
	e.queries++
	return e.Executor.QueryJSON(ctx, database, query, maxBytes)
}

func TestOMORecordedPlanChangeToken(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "sessions")
	cwd := t.TempDir()
	session := "omo-live"
	path := filepath.Join(root, encodeOMOCWD(cwd), "2026-10-20T12-00-00-000Z_"+session+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(agentroots.OMOListEnv, home)
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := fmt.Fprintf(file, "{\"type\":\"session\",\"id\":%q,\"cwd\":%q}\n", session, cwd); err != nil {
		t.Fatal(err)
	}
	reader := NewReader(home)
	scope := BrowseScope{Provider: "Oh My OpenCode", CWD: cwd, SessionID: session}
	before, err := reader.ChangeToken(t.Context(), scope)
	if err != nil || before == "" {
		t.Fatalf("missing OMO change token: %q %v", before, err)
	}
	if _, err := file.WriteString("{\"type\":\"custom\",\"customType\":\"senpi.todo-state\",\"data\":{\"schema\":\"v2\",\"phases\":[{\"name\":\"Build\",\"tasks\":[{\"content\":\"Ship\",\"status\":\"pending\"}]}]}}\n"); err != nil {
		t.Fatal(err)
	}
	after, err := reader.ChangeToken(t.Context(), scope)
	page, readErr := reader.ReadFor(scope.Provider, cwd, session, "", 10)
	if err != nil || readErr != nil || before == after || page.OMOPlan == nil || len(page.OMOPlan.Phases) != 1 {
		t.Fatalf("OMO plan did not update: %+v %v %v", page, err, readErr)
	}
}

func TestDatabaseChangeTokenIdleAndReplacement(t *testing.T) {
	for _, provider := range []string{"opencode", "hermes"} {
		t.Run(provider, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv(agentroots.OpenCodeListEnv, root)
			t.Setenv(agentroots.HermesListEnv, root)
			t.Setenv("HERMES_HOME", "")
			name := "opencode.db"
			if provider == "hermes" {
				name = "state.db"
			}
			path := filepath.Join(root, name)
			if err := os.WriteFile(path, make([]byte, 4096), 0o400); err != nil {
				t.Fatal(err)
			}
			reader := NewReader(t.TempDir())
			executor := &changeCountingExecutor{Executor: sqliteexec.MustFromEnv()}
			reader.openCode.executor, reader.hermes.executor = executor, executor
			scope := BrowseScope{Provider: provider, SessionID: "ses_idle123"}
			before, err := reader.ChangeToken(t.Context(), scope)
			if err != nil || before == "" {
				t.Fatalf("missing database change token: %q %v", before, err)
			}
			start := time.Now()
			for range 1000 {
				token, err := reader.ChangeToken(t.Context(), scope)
				if err != nil || token != before {
					t.Fatalf("idle token changed: %q %v", token, err)
				}
			}
			t.Logf("idle provider=%s polls=1000 queries=%d elapsed=%s per_poll=%s", provider, executor.queries, time.Since(start), time.Since(start)/1000)
			if executor.queries != 0 {
				t.Fatal("idle polling executed SQL")
			}
			info, _ := os.Stat(path)
			if err := os.WriteFile(path+".next", make([]byte, 4096), 0o400); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(path+".next", info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(path+".next", path); err != nil {
				t.Fatal(err)
			}
			after, err := reader.ChangeToken(t.Context(), scope)
			if err != nil || after == before {
				t.Fatal("same-size/time replacement did not wake feed")
			}
			raw, _ := os.ReadFile(path)
			if sha256.Sum256(raw) != sha256.Sum256(make([]byte, 4096)) {
				t.Fatal("polling modified source")
			}
			if err := os.WriteFile(path+"-wal", make([]byte, 32), 0o600); err != nil {
				t.Fatal(err)
			}
			wal, _ := reader.ChangeToken(t.Context(), scope)
			if wal == after {
				t.Fatal("WAL creation did not wake feed")
			}
			cancelled, cancel := context.WithCancel(t.Context())
			cancel()
			if _, err := reader.ChangeToken(cancelled, scope); err == nil {
				t.Fatal("cancelled polling ignored context")
			}
		})
	}
}

func TestDatabaseReadsFollowCommitPublishedAfterItsWALWrite(t *testing.T) {
	root := t.TempDir()
	t.Setenv(agentroots.OpenCodeListEnv, root)
	session := "ses_live123"
	database := filepath.Join(root, "opencode.db")
	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("PRAGMA journal_mode=WAL")
	exec(`CREATE TABLE session_v2(id TEXT PRIMARY KEY,directory TEXT,title TEXT,time_updated INTEGER,agent TEXT); CREATE TABLE session_message(id TEXT PRIMARY KEY,session_id TEXT,type TEXT,seq INTEGER,time_created INTEGER,data TEXT); CREATE INDEX session_order ON session_message(session_id,seq);`)
	exec("INSERT INTO session_v2 VALUES(?,?,'Live',1,'opencode')", session, root)
	appendMessage := func(index int, text string) {
		t.Helper()
		exec("INSERT INTO session_message VALUES(?,?,'assistant',?,?,?)", fmt.Sprintf("m%04d", index), session, index, index, `{"content":[{"type":"text","text":"`+text+`"}]}`)
	}
	appendMessage(1, "seed")
	reader := NewReader(t.TempDir())
	scope := BrowseScope{Provider: "opencode", SessionID: session}
	read := func() []Entry {
		t.Helper()
		page, err := reader.ReadFor(scope.Provider, root, session, "", 10)
		if err != nil || !page.Available {
			t.Fatalf("read failed: %+v %v", page, err)
		}
		return page.Entries
	}
	if entries := read(); len(entries) != 1 {
		t.Fatalf("seed entries = %+v", entries)
	}

	shm, err := os.OpenFile(database+"-shm", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer shm.Close()
	var published, unpublished [96]byte
	if _, err := shm.ReadAt(unpublished[:], 0); err != nil {
		t.Fatal(err)
	}
	appendMessage(2, "appended")
	if _, err := shm.ReadAt(published[:], 0); err != nil || published == unpublished {
		t.Fatalf("commit did not change the WAL index: %v", err)
	}
	// A writer appends its WAL frames before this header publishes them to readers.
	if _, err := shm.WriteAt(unpublished[:], 0); err != nil {
		t.Fatal(err)
	}
	pending, err := reader.ChangeToken(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	if entries := read(); len(entries) != 1 {
		t.Fatalf("reader saw the unpublished commit: %+v", entries)
	}
	if _, err := shm.WriteAt(published[:], 0); err != nil {
		t.Fatal(err)
	}
	current, err := reader.ChangeToken(t.Context(), scope)
	if err != nil || current == pending {
		t.Fatalf("publishing the commit left the change token unchanged: %v", err)
	}
	if entries := read(); len(entries) != 2 || entries[1].Text != "appended" {
		t.Fatalf("read after publication = %+v", entries)
	}
}
