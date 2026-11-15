package conversation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
)

func TestBrowserUsesNativeHermesCursorPaging(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "workspace")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	const sessionID = "20260812_100000_browser"
	database := filepath.Join(root, "state.db")
	messageSQL := fmt.Sprintf(`INSERT INTO messages(session_id,role,content,timestamp,active,compacted) VALUES
	('%s','user','older',100,1,0),
	('%s','assistant','newer',101,1,0);`, sessionID, sessionID)
	createHermesTestDatabase(t, database, sessionID, cwd, "Browser", messageSQL)
	t.Setenv(agentroots.HermesListEnv, root)
	t.Setenv("HERMES_HOME", "")
	reader := NewReader(t.TempDir())
	browser := newTestBrowser(t, reader, DefaultBrowserOptions())
	scope := BrowseScope{Provider: "hermes", CWD: cwd, SessionID: sessionID}
	latest, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || latest.Mode != BrowseNative || len(latest.Entries) != 1 || !latest.HasMore || latest.NextCursor == "" || latest.Entries[0].Text != "newer" || latest.SourceRevision == "" {
		t.Fatalf("Hermes latest page = %#v, err = %v", latest, err)
	}
	execTestSQL(t, database, fmt.Sprintf("INSERT INTO messages(session_id,role,content,timestamp,active,compacted) VALUES('%s','assistant','appended',102,1,0);", sessionID))
	execTestSQL(t, database, fmt.Sprintf("UPDATE messages SET active=0,compacted=1 WHERE session_id='%s' AND content='newer'; INSERT INTO messages(session_id,role,content,timestamp,active,compacted) VALUES('%s','assistant','newer',101,1,0);", sessionID, sessionID))
	older, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: latest.NextCursor, Limit: 1})
	if err != nil || older.Mode != BrowseNative || older.HasMore || len(older.Entries) != 1 || older.Entries[0].Text != "older" {
		t.Fatalf("Hermes older page after append = %#v, err = %v", older, err)
	}
}

func hermesBrowserFixture(t *testing.T, sessionID, messageSQL string) (*Browser, BrowseScope, string) {
	t.Helper()
	root := t.TempDir()
	cwd := filepath.Join(root, "workspace")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(root, "state.db")
	createHermesTestDatabase(t, database, sessionID, cwd, "Browser", messageSQL)
	t.Setenv(agentroots.HermesListEnv, root)
	t.Setenv("HERMES_HOME", "")
	browser := newTestBrowser(t, NewReader(t.TempDir()), DefaultBrowserOptions())
	return browser, BrowseScope{Provider: "hermes", CWD: cwd, SessionID: sessionID}, database
}

func TestBrowserHermesRejectsAnotherWorkspace(t *testing.T) {
	const sessionID = "20260812_110000_abcdef"
	browser, scope, _ := hermesBrowserFixture(t, sessionID, fmt.Sprintf(
		"INSERT INTO messages(session_id,role,content,timestamp,active,compacted) VALUES('%s','user','question',100,1,0);", sessionID))
	if page, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 80}); err != nil || !page.Available || len(page.Entries) != 1 {
		t.Fatalf("own workspace page = %#v, err = %v", page, err)
	}
	scope.CWD = filepath.Join(filepath.Dir(scope.CWD), "other")
	if err := os.MkdirAll(scope.CWD, 0o700); err != nil {
		t.Fatal(err)
	}
	page, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 80})
	if err != nil || page.Available || page.ReasonCode != "invalid_session" || len(page.Entries) != 0 {
		t.Fatalf("other workspace page = %#v, err = %v", page, err)
	}
}

func TestBrowserHermesPreservesCompactedLogicalOrder(t *testing.T) {
	const sessionID = "20260812_120000_abcdef"
	browser, scope, _ := hermesBrowserFixture(t, sessionID, fmt.Sprintf(`
INSERT INTO messages(session_id,role,content,timestamp,active,compacted) VALUES
 ('%[1]s','assistant','A',100,0,1),
 ('%[1]s','assistant','B',100,0,1),
 ('%[1]s','assistant','C',100,1,0),
 ('%[1]s','assistant','A',100,1,0),
 ('%[1]s','assistant','B',100,1,0);`, sessionID))
	all, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 80})
	if err != nil || !all.Available || all.Total == nil || *all.Total != 3 || len(all.Entries) != 3 {
		t.Fatalf("compacted page = %#v, err = %v", all, err)
	}
	if got := []string{all.Entries[0].Text, all.Entries[1].Text, all.Entries[2].Text}; !reflect.DeepEqual(got, []string{"A", "B", "C"}) {
		t.Fatalf("compacted order = %v, want [A B C]", got)
	}
	cursor := ""
	for _, want := range []struct {
		text    string
		hasMore bool
	}{{"C", true}, {"B", true}, {"A", false}} {
		page, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: cursor, Limit: 1})
		if err != nil || len(page.Entries) != 1 || page.Entries[0].Text != want.text || page.HasMore != want.hasMore {
			t.Fatalf("page for %q = %#v, err = %v", want.text, page, err)
		}
		cursor = page.NextCursor
	}
}

func TestBrowserHermesResolvesArchivedCursorAfterCompaction(t *testing.T) {
	const sessionID = "20260812_125000_abcdef"
	browser, scope, database := hermesBrowserFixture(t, sessionID, fmt.Sprintf(`
INSERT INTO messages(session_id,role,content,timestamp,active,compacted) VALUES
 ('%[1]s','assistant','A',100,1,0),
 ('%[1]s','assistant','B',200,1,0);`, sessionID))
	latest, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || len(latest.Entries) != 1 || latest.Entries[0].Text != "B" || latest.Entries[0].ID != "2" || !latest.HasMore {
		t.Fatalf("initial page = %#v, err = %v", latest, err)
	}
	execTestSQL(t, database, fmt.Sprintf(`
BEGIN;
UPDATE messages SET active=0,compacted=1 WHERE session_id='%[1]s' AND active=1;
INSERT INTO messages(session_id,role,content,timestamp,active,compacted)
VALUES('%[1]s','assistant','B',200,1,0);
COMMIT;`, sessionID))
	older, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: latest.NextCursor, Limit: 1})
	if err != nil || !older.Available || len(older.Entries) != 1 || older.Entries[0].Text != "A" || older.Entries[0].ID != "1" || older.HasMore {
		t.Fatalf("archived cursor page = %#v, err = %v", older, err)
	}
}

func TestBrowserHermesFiltersHiddenMessagesBeforePaging(t *testing.T) {
	const sessionID = "20260812_130000_abcdef"
	browser, scope, _ := hermesBrowserFixture(t, sessionID, fmt.Sprintf(`
INSERT INTO messages(session_id,role,content,timestamp,active,compacted,display_kind) VALUES
 ('%[1]s','user','visible question',100,1,0,NULL),
 ('%[1]s','user','internal handoff',200,0,1,'hidden'),
 ('%[1]s','assistant','visible answer',300,1,0,NULL);`, sessionID))
	page, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || !page.Available || page.Total == nil || *page.Total != 2 || len(page.Entries) != 1 || page.Entries[0].Text != "visible answer" || !page.HasMore {
		t.Fatalf("hidden-message page = %#v, err = %v", page, err)
	}
	older, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: page.NextCursor, Limit: 1})
	if err != nil || older.Total == nil || *older.Total != 2 || len(older.Entries) != 1 || older.Entries[0].Text != "visible question" || older.HasMore {
		t.Fatalf("hidden-message older page = %#v, err = %v", older, err)
	}
}

func TestBrowserHermesCorruptRowsStillReturnAContinuationPage(t *testing.T) {
	const sessionID = "20260812_130000_corrupt"
	browser, scope, _ := hermesBrowserFixture(t, sessionID, fmt.Sprintf(
		"INSERT INTO messages(session_id,role,content,tool_calls,timestamp,active,compacted) VALUES('%s','assistant','', 'not-json',100,1,0);", sessionID))
	page, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || !page.Available || page.Diagnostics.CorruptRecords != 1 || len(page.Entries) != 0 {
		t.Fatalf("corrupt Hermes page = %#v, err = %v", page, err)
	}
}

func TestBrowserHermesRejectsForgedCursor(t *testing.T) {
	const sessionID = "20260812_131000_abcdef"
	browser, scope, _ := hermesBrowserFixture(t, sessionID, fmt.Sprintf(`
INSERT INTO messages(session_id,role,content,timestamp,active,compacted) VALUES
 ('%[1]s','user','one',100,1,0),
 ('%[1]s','assistant','two',101,1,0);`, sessionID))
	latest, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || latest.SourceRevision == "" {
		t.Fatalf("latest page = %#v, err = %v", latest, err)
	}
	page, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: signedNativeCursor(t, browser, scope, latest.SourceRevision, "999"), Limit: 1})
	if err != nil || page.State != BrowseFailed || page.ReasonCode != "invalid_cursor" {
		t.Fatalf("unknown cursor page = %#v, err = %v", page, err)
	}
}

func TestHermesReaderBindsWorkspace(t *testing.T) {
	const sessionID = "20260812_110000_abcdef"
	browser, scope, _ := hermesBrowserFixture(t, sessionID, fmt.Sprintf(
		"INSERT INTO messages(session_id,role,content,timestamp,active,compacted) VALUES('%s','user','question',100,1,0);", sessionID))
	other := filepath.Join(filepath.Dir(scope.CWD), "other")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	reader := browser.reader
	own, err := reader.ReadLatest("hermes", ProjectContext{CWD: scope.CWD}, sessionID, 80)
	if err != nil || len(own) != 1 {
		t.Fatalf("own workspace page = %#v, err = %v", own, err)
	}
	wrongWorkspace, err := reader.ReadLatest("hermes", ProjectContext{CWD: other}, sessionID, 80)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Hermes wrong-workspace page = %#v, err = %v", wrongWorkspace, err)
	}
}

func TestHermesReaderReadsStateDatabaseConversation(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "workspace")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	const sessionID = "20260812_100000_abcdef"
	database := filepath.Join(root, "state.db")
	sql := fmt.Sprintf(`BEGIN;
CREATE TABLE sessions(id TEXT PRIMARY KEY,cwd TEXT,title TEXT);
CREATE TABLE messages(
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 session_id TEXT NOT NULL,
 role TEXT NOT NULL,
 content TEXT,
 tool_call_id TEXT,
 tool_calls TEXT,
 tool_name TEXT,
 timestamp REAL NOT NULL,
 active INTEGER NOT NULL DEFAULT 1,
 compacted INTEGER NOT NULL DEFAULT 0,
 display_kind TEXT
);
INSERT INTO sessions VALUES('%s','%s','Hermes title');
INSERT INTO messages(session_id,role,content,timestamp,active,compacted)
 VALUES('%s','user','question',100,1,0);
INSERT INTO messages(session_id,role,content,tool_calls,timestamp,active,compacted)
 VALUES('%s','assistant',char(0)||'json:'||'[{
  "type":"text","text":"answer"
}]','[{"id":"call_1","type":"function","function":{"name":"terminal","arguments":"{\"command\":\"pwd\"}"}}]',101,1,0);
INSERT INTO messages(session_id,role,content,tool_call_id,tool_name,timestamp,active,compacted)
 VALUES('%s','tool','command output','call_1','terminal',102,1,0);
COMMIT;`, sessionID, cwd, sessionID, sessionID, sessionID)
	execTestSQL(t, database, sql)
	t.Setenv(agentroots.HermesListEnv, root)
	t.Setenv("HERMES_HOME", "")
	reader := NewReader(t.TempDir())

	page, err := reader.ReadLatest("hermes-agent", ProjectContext{CWD: cwd}, sessionID, 80)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 {
		t.Fatalf("Hermes page entries=%d, want two conversation turns: %#v", len(page), page)
	}
	if page[0].Role != "user" || page[0].Text != "question" {
		t.Fatalf("Hermes user entry = %#v", page[0])
	}
	assistant := page[1]
	if assistant.Role != "assistant" || assistant.Text != "answer" || len(assistant.Tools) != 1 {
		t.Fatalf("Hermes assistant entry = %#v", assistant)
	}
	tool := assistant.Tools[0]
	if tool.ID != "call_1" || tool.Name != "terminal" || tool.Input != `{"command":"pwd"}` || tool.Output != "command output" {
		t.Fatalf("Hermes tool activity = %#v", tool)
	}
	if assistant.Timestamp != "1970-01-01T00:01:41Z" {
		t.Fatalf("Hermes assistant timestamp = %q", assistant.Timestamp)
	}
	location := reader.LocateWithProject("hermes", ProjectContext{CWD: cwd}, sessionID)
	if location.Path != database || location.Title != "Hermes title" {
		t.Fatalf("Hermes location = %#v", location)
	}
}

func TestHermesDirectoryBindingResolvesSymlinks(t *testing.T) {
	realDirectory := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(realDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "project-link")
	if err := os.Symlink(realDirectory, link); err != nil {
		t.Fatal(err)
	}
	if !sameHermesDirectory(link, realDirectory) {
		t.Fatal("symlinked workspace did not match its real Hermes directory")
	}
	other := filepath.Join(t.TempDir(), "other")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if sameHermesDirectory(other, realDirectory) {
		t.Fatal("different workspace matched Hermes session directory")
	}
}

const hermesTestMessageSchema = `CREATE TABLE messages(
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 session_id TEXT NOT NULL,
 role TEXT NOT NULL,
 content TEXT,
 tool_call_id TEXT,
 tool_calls TEXT,
 tool_name TEXT,
 timestamp REAL NOT NULL,
 active INTEGER NOT NULL DEFAULT 1,
 compacted INTEGER NOT NULL DEFAULT 0,
 display_kind TEXT
);`

func createHermesTestDatabase(t *testing.T, database, sessionID, cwd, title, messageSQL string) {
	t.Helper()
	sql := fmt.Sprintf(`BEGIN;
CREATE TABLE sessions(id TEXT PRIMARY KEY,cwd TEXT,title TEXT);
%s
INSERT INTO sessions VALUES('%s','%s','%s');
%s
COMMIT;`, hermesTestMessageSchema, sessionID, cwd, title, messageSQL)
	execTestSQL(t, database, sql)
}

func TestHermesReaderUsesCachedDatabaseForTitleAndContent(t *testing.T) {
	base := t.TempDir()
	rootA := filepath.Join(base, "a")
	rootB := filepath.Join(base, "b")
	cwd := filepath.Join(base, "workspace")
	for _, path := range []string{rootA, rootB, cwd} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	const sessionID = "20260812_140000_abcdef"
	databaseB := filepath.Join(rootB, "state.db")
	createHermesTestDatabase(t, databaseB, sessionID, cwd, "Title B",
		fmt.Sprintf("INSERT INTO messages(session_id,role,content,timestamp,active,compacted) VALUES('%s','assistant','Transcript B',100,1,0);", sessionID))
	t.Setenv(agentroots.HermesListEnv, strings.Join([]string{rootA, rootB}, string(os.PathListSeparator)))
	t.Setenv("HERMES_HOME", "")
	reader := NewReader(t.TempDir())

	location := reader.LocateWithProject("hermes-agent", ProjectContext{CWD: cwd}, sessionID)
	if location.Path != databaseB || location.Title != "Title B" {
		t.Fatalf("Hermes initial location = %#v, want database B", location)
	}

	databaseA := filepath.Join(rootA, "state.db")
	createHermesTestDatabase(t, databaseA, sessionID, cwd, "Title A",
		fmt.Sprintf("INSERT INTO messages(session_id,role,content,timestamp,active,compacted) VALUES('%s','assistant','Transcript A',100,1,0);", sessionID))
	entries, err := reader.ReadLatest("hermes", ProjectContext{CWD: cwd}, sessionID, 80)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Text != "Transcript B" {
		t.Fatalf("Hermes cached transcript = %#v, want database B content", entries)
	}
}
