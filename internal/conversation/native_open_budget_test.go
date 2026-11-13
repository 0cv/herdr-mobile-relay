package conversation

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unsafe"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
	"github.com/0cv/herdr-mobile-relay/internal/sqliteexec"
	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

type nativeWorkExecutor struct {
	t        *testing.T
	delegate sqliteexec.Executor
	steps    int64
	queries  int
}

func (r *nativeWorkExecutor) QueryJSON(ctx context.Context, database, query string, maxBytes int) ([]byte, error) {
	raw, err := r.delegate.QueryJSON(ctx, database, query, maxBytes)
	if err != nil {
		return raw, err
	}
	r.steps += sqliteVMSteps(r.t, database, query)
	r.queries++
	return raw, nil
}

func sqliteVMSteps(t *testing.T, database, query string) int64 {
	t.Helper()
	tls := libc.NewTLS()
	defer tls.Close()
	filename, err := libc.CString(database)
	if err != nil {
		t.Fatal(err)
	}
	defer libc.Xfree(tls, filename)
	sqlText, err := libc.CString(query)
	if err != nil {
		t.Fatal(err)
	}
	defer libc.Xfree(tls, sqlText)
	slots := tls.Alloc(2 * int(unsafe.Sizeof(uintptr(0))))
	defer tls.Free(2 * int(unsafe.Sizeof(uintptr(0))))
	if rc := sqlite3.Xsqlite3_open_v2(tls, filename, slots, sqlite3.SQLITE_OPEN_READONLY, 0); rc != sqlite3.SQLITE_OK {
		t.Fatalf("open SQLite work counter: %d", rc)
	}
	db := libc.AtomicLoadPUintptr(slots)
	defer sqlite3.Xsqlite3_close(tls, db)
	statementSlot := slots + unsafe.Sizeof(uintptr(0))
	if rc := sqlite3.Xsqlite3_prepare_v2(tls, db, sqlText, -1, statementSlot, 0); rc != sqlite3.SQLITE_OK {
		t.Fatalf("prepare SQLite work counter: %s", libc.GoString(sqlite3.Xsqlite3_errmsg(tls, db)))
	}
	statement := libc.AtomicLoadPUintptr(statementSlot)
	defer sqlite3.Xsqlite3_finalize(tls, statement)
	for {
		switch rc := sqlite3.Xsqlite3_step(tls, statement); rc {
		case sqlite3.SQLITE_ROW:
		case sqlite3.SQLITE_DONE:
			return int64(sqlite3.Xsqlite3_stmt_status(tls, statement, sqlite3.SQLITE_STMTSTATUS_VM_STEP, 0))
		default:
			t.Fatalf("measure SQLite work: %s", libc.GoString(sqlite3.Xsqlite3_errmsg(tls, db)))
		}
	}
}

func nativeWorkDatabase(t *testing.T, provider string, count int, directory string) string {
	t.Helper()
	name, schema, session := "opencode.db", openCodeV1Schema, "ses_nativebudget"
	if provider == "opencode-v2" {
		schema = openCodeV2Schema
	}
	if provider == "hermes" {
		name = "state.db"
		schema = "CREATE TABLE sessions(id TEXT PRIMARY KEY,cwd TEXT,title TEXT);" + hermesTestMessageSchema
		session = "20261023_123456_native"
	}
	db, err := sql.Open("sqlite", filepath.Join(directory, name))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	insert := "INSERT INTO message(id,session_id,time_created,data) VALUES(?,?,?,?)"
	switch provider {
	case "opencode-v1":
		_, err = tx.Exec("INSERT INTO session VALUES(?, '/synthetic', 'Budget', 1, 'opencode')", session)
	case "opencode-v2":
		_, err = tx.Exec("INSERT INTO session_v2(id,directory,title) VALUES(?, '/synthetic', 'Budget')", session)
		insert = "INSERT INTO session_message(id,session_id,seq,time_created,type,data) VALUES(?,?,?,?, 'user', ?)"
	case "hermes":
		_, err = tx.Exec("INSERT INTO sessions VALUES(?, '/synthetic', 'Budget')", session)
		insert = "INSERT INTO messages(session_id,role,content,timestamp,active,compacted) VALUES(?, 'assistant', ?, ?, 1, 0)"
	}
	if err != nil {
		t.Fatal(err)
	}
	statement, err := tx.Prepare(insert)
	if err != nil {
		t.Fatal(err)
	}
	defer statement.Close()
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("message-%08d", i)
		text := fmt.Sprintf("turn-%08d:%s", i, strings.Repeat("x", 128))
		switch provider {
		case "opencode-v1":
			_, err = statement.Exec(id, session, i, `{"role":"user"}`)
			if err == nil {
				_, err = tx.Exec("INSERT INTO part VALUES(?,?,?)", "part-"+id, id, `{"type":"text","text":"`+text+`"}`)
			}
		case "opencode-v2":
			_, err = statement.Exec(id, session, i, i, `{"text":"`+text+`"}`)
		case "hermes":
			_, err = statement.Exec(session, text, i+1)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return session
}

func measureNativeOpening(t *testing.T, provider string, count int) int64 {
	t.Helper()
	directory := t.TempDir()
	session := nativeWorkDatabase(t, provider, count, directory)
	recorder := &nativeWorkExecutor{t: t, delegate: sqliteexec.New()}
	reader := NewReader(t.TempDir())
	reader.openCode.executor, reader.hermes.executor = recorder, recorder
	t.Setenv(agentroots.OpenCodeListEnv, directory)
	t.Setenv(agentroots.HermesListEnv, directory)
	agent := "opencode"
	if provider == "hermes" {
		agent = "hermes"
	}
	browser, err := NewBrowser(reader, t.TempDir(), DefaultBrowserOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	request := BrowseRequest{Scope: BrowseScope{Provider: agent, SessionID: session, PaneID: "synthetic-pane"}, Limit: 24}
	page, err := browser.ReadPage(t.Context(), request)
	if err != nil || !page.Available || page.Error != nil || len(page.Entries) != 24 || !page.HasMore || page.NextCursor == "" {
		t.Fatalf("page=%+v error=%v", page, err)
	}
	if provider == "opencode-v2" && page.Total != nil {
		t.Fatalf("count-free page claimed a total: %d", *page.Total)
	}
	coldSteps, coldQueries := recorder.steps, recorder.queries
	warm, err := browser.ReadPage(t.Context(), request)
	if err != nil || !warm.Available || len(warm.Entries) != 24 || warm.Entries[0].ID != page.Entries[0].ID {
		t.Fatalf("warm=%+v error=%v", warm, err)
	}
	t.Logf("NATIVE_OPEN_WORK provider=%s records=%d entries=24 cold_vm_steps=%d cold_queries=%d repeated_open_vm_steps=%d", provider, count, coldSteps, coldQueries, recorder.steps-coldSteps)
	return coldSteps
}

func TestNativeColdOpeningMeasurements(t *testing.T) {
	for _, provider := range []string{"opencode-v1", "opencode-v2", "hermes"} {
		for _, count := range []int{256, 4096, 16384} {
			t.Run(fmt.Sprintf("%s/%d", provider, count), func(t *testing.T) {
				measureNativeOpening(t, provider, count)
			})
		}
	}
}

func TestNativeColdOpeningWorkBudget(t *testing.T) {
	for _, count := range []int{256, 4096, 16384} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			if steps := measureNativeOpening(t, "opencode-v2", count); steps > 2000 {
				t.Fatalf("count-free indexed v2 recent open used %d VM steps for 24 entries", steps)
			}
		})
	}
}

func TestPageUnknownTotalJSON(t *testing.T) {
	for _, total := range []int{UnknownTotal, 0, 7} {
		raw, err := json.Marshal(Page{Available: true, Total: total, HasMore: true, Entries: []Entry{{ID: "turn", Role: "user", Text: "hello"}}})
		if err != nil {
			t.Fatal(err)
		}
		var page struct {
			Total     *int    `json:"total"`
			Available bool    `json:"available"`
			HasMore   bool    `json:"has_more"`
			Entries   []Entry `json:"entries"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			t.Fatal(err)
		}
		if !page.Available || !page.HasMore || len(page.Entries) != 1 {
			t.Fatalf("lost page fields: %s", raw)
		}
		if total == UnknownTotal {
			if page.Total != nil {
				t.Fatalf("unknown total is not null: %s", raw)
			}
		} else if page.Total == nil || *page.Total != total {
			t.Fatalf("lost exact total: %s", raw)
		}
	}
}
