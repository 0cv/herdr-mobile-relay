package conversation

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
)

func TestHermesReaderReadsStateDatabaseConversation(t *testing.T) {
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 is unavailable")
	}
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
	command := exec.Command(sqlite, database)
	command.Stdin = strings.NewReader(sql)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create Hermes database: %v: %s", err, output)
	}
	t.Setenv(agentroots.HermesListEnv, root)
	t.Setenv("HERMES_HOME", "")
	reader := NewReader(t.TempDir())
	reader.hermes.binary = sqlite

	page, err := reader.ReadFor("hermes-agent", cwd, sessionID, "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if !page.Available || page.ReasonCode != "" {
		t.Fatalf("Hermes page unavailable: %#v", page)
	}
	if page.Total != 2 || len(page.Entries) != 2 {
		t.Fatalf("Hermes page entries=%d total=%d, want two conversation turns: %#v", len(page.Entries), page.Total, page.Entries)
	}
	if page.Entries[0].Role != "user" || page.Entries[0].Text != "question" {
		t.Fatalf("Hermes user entry = %#v", page.Entries[0])
	}
	assistant := page.Entries[1]
	if assistant.Role != "assistant" || assistant.Text != "answer" || len(assistant.Tools) != 1 {
		t.Fatalf("Hermes assistant entry = %#v", assistant)
	}
	tool := assistant.Tools[0]
	if tool.ID != "call_1" || tool.Name != "terminal" || tool.Input != `{"command":"pwd"}` || tool.Output != "command output" {
		t.Fatalf("Hermes tool activity = %#v", tool)
	}
	if assistant.Timestamp == "" {
		t.Fatal("Hermes assistant timestamp is empty")
	}
	location := reader.Locate("hermes", cwd, sessionID)
	if location.Path != database || location.Title != "Hermes title" {
		t.Fatalf("Hermes location = %#v", location)
	}
}

func TestHermesReaderPagesByMessageIDAndBindsWorkspace(t *testing.T) {
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 is unavailable")
	}
	root := t.TempDir()
	cwd := filepath.Join(root, "workspace")
	other := filepath.Join(root, "other")
	for _, path := range []string{cwd, other} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	const sessionID = "20260812_110000_abcdef"
	database := filepath.Join(root, "state.db")
	sql := fmt.Sprintf(`CREATE TABLE sessions(id TEXT PRIMARY KEY,cwd TEXT,title TEXT);
CREATE TABLE messages(id INTEGER PRIMARY KEY AUTOINCREMENT,session_id TEXT NOT NULL,role TEXT NOT NULL,content TEXT,tool_call_id TEXT,tool_calls TEXT,tool_name TEXT,timestamp REAL NOT NULL,active INTEGER NOT NULL DEFAULT 1,compacted INTEGER NOT NULL DEFAULT 0,display_kind TEXT);
INSERT INTO sessions VALUES('%s','%s','Paging');
INSERT INTO messages(session_id,role,content,timestamp,active,compacted) VALUES('%s','user','question',100,1,0);
INSERT INTO messages(session_id,role,content,timestamp,active,compacted) VALUES('%s','assistant','answer',101,1,0);`, sessionID, cwd, sessionID, sessionID)
	command := exec.Command(sqlite, database)
	command.Stdin = strings.NewReader(sql)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create Hermes database: %v: %s", err, output)
	}
	t.Setenv(agentroots.HermesListEnv, root)
	t.Setenv("HERMES_HOME", "")
	reader := NewReader(t.TempDir())
	reader.hermes.binary = sqlite

	latest, err := reader.ReadFor("hermes", cwd, sessionID, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !latest.Available || !latest.HasMore || len(latest.Entries) != 1 || latest.Entries[0].Text != "answer" {
		t.Fatalf("Hermes latest page = %#v", latest)
	}
	older, err := reader.ReadFor("hermes", cwd, sessionID, latest.Entries[0].ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !older.Available || older.HasMore || len(older.Entries) != 1 || older.Entries[0].Text != "question" {
		t.Fatalf("Hermes older page = %#v", older)
	}
	wrongWorkspace, err := reader.ReadFor("hermes", other, sessionID, "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if wrongWorkspace.Available || wrongWorkspace.ReasonCode != "invalid_session" {
		t.Fatalf("Hermes wrong-workspace page = %#v", wrongWorkspace)
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

func createHermesTestDatabase(t *testing.T, sqlite, database, sessionID, cwd, title, messageSQL string) {
	t.Helper()
	sql := fmt.Sprintf(`BEGIN;
CREATE TABLE sessions(id TEXT PRIMARY KEY,cwd TEXT,title TEXT);
%s
INSERT INTO sessions VALUES('%s','%s','%s');
%s
COMMIT;`, hermesTestMessageSchema, sessionID, cwd, title, messageSQL)
	command := exec.Command(sqlite, database)
	command.Stdin = strings.NewReader(sql)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create Hermes database: %v: %s", err, output)
	}
}

func TestHermesReaderPreservesCompactedLogicalOrder(t *testing.T) {
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 is unavailable")
	}
	root := t.TempDir()
	cwd := filepath.Join(root, "workspace")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	const sessionID = "20260812_120000_abcdef"
	database := filepath.Join(root, "state.db")
	messageSQL := fmt.Sprintf(`
INSERT INTO messages(session_id,role,content,timestamp,active,compacted) VALUES
 ('%s','assistant','A',100,0,1),
 ('%s','assistant','B',200,0,1),
 ('%s','assistant','C',300,1,0),
 ('%s','assistant','A',100,0,1);`, sessionID, sessionID, sessionID, sessionID)
	createHermesTestDatabase(t, sqlite, database, sessionID, cwd, "Compacted order", messageSQL)
	t.Setenv(agentroots.HermesListEnv, root)
	t.Setenv("HERMES_HOME", "")
	reader := NewReader(t.TempDir())
	reader.hermes.binary = sqlite

	all, err := reader.ReadFor("hermes", cwd, sessionID, "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if !all.Available || all.Total != 3 || len(all.Entries) != 3 {
		t.Fatalf("Hermes compacted page = %#v, want three entries", all)
	}
	if got := []string{all.Entries[0].Text, all.Entries[1].Text, all.Entries[2].Text}; !reflect.DeepEqual(got, []string{"A", "B", "C"}) {
		t.Fatalf("Hermes compacted order = %v, want [A B C]", got)
	}

	latest, err := reader.ReadFor("hermes", cwd, sessionID, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(latest.Entries) != 1 || latest.Entries[0].Text != "C" || !latest.HasMore {
		t.Fatalf("Hermes latest compacted page = %#v", latest)
	}
	older, err := reader.ReadFor("hermes", cwd, sessionID, latest.Entries[0].ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(older.Entries) != 1 || older.Entries[0].Text != "B" || !older.HasMore {
		t.Fatalf("Hermes older compacted page = %#v", older)
	}
	oldest, err := reader.ReadFor("hermes", cwd, sessionID, older.Entries[0].ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(oldest.Entries) != 1 || oldest.Entries[0].Text != "A" || oldest.HasMore {
		t.Fatalf("Hermes oldest compacted page = %#v", oldest)
	}
}

func TestHermesReaderFiltersHiddenMessagesBeforePaging(t *testing.T) {
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 is unavailable")
	}
	root := t.TempDir()
	cwd := filepath.Join(root, "workspace")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	const sessionID = "20260812_130000_abcdef"
	database := filepath.Join(root, "state.db")
	messageSQL := fmt.Sprintf(`
INSERT INTO messages(session_id,role,content,timestamp,active,compacted,display_kind) VALUES
 ('%s','user','visible question',100,1,0,NULL),
 ('%s','user','internal handoff',200,0,1,'hidden'),
 ('%s','assistant','visible answer',300,1,0,NULL);`, sessionID, sessionID, sessionID)
	createHermesTestDatabase(t, sqlite, database, sessionID, cwd, "Hidden rows", messageSQL)
	t.Setenv(agentroots.HermesListEnv, root)
	t.Setenv("HERMES_HOME", "")
	reader := NewReader(t.TempDir())
	reader.hermes.binary = sqlite

	page, err := reader.ReadFor("hermes", cwd, sessionID, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !page.Available || page.Total != 2 || len(page.Entries) != 1 || page.Entries[0].Text != "visible answer" {
		t.Fatalf("Hermes hidden-message page = %#v", page)
	}
	older, err := reader.ReadFor("hermes", cwd, sessionID, page.Entries[0].ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if older.Total != 2 || len(older.Entries) != 1 || older.Entries[0].Text != "visible question" || older.HasMore {
		t.Fatalf("Hermes hidden-message older page = %#v", older)
	}
}

func TestHermesReaderUsesCachedDatabaseForTitleAndContent(t *testing.T) {
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 is unavailable")
	}
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
	createHermesTestDatabase(t, sqlite, databaseB, sessionID, cwd, "Title B",
		fmt.Sprintf("INSERT INTO messages(session_id,role,content,timestamp,active,compacted) VALUES('%s','assistant','Transcript B',100,1,0);", sessionID))
	t.Setenv(agentroots.HermesListEnv, strings.Join([]string{rootA, rootB}, string(os.PathListSeparator)))
	t.Setenv("HERMES_HOME", "")
	reader := NewReader(t.TempDir())
	reader.hermes.binary = sqlite

	location := reader.Locate("hermes-agent", cwd, sessionID)
	if location.Path != databaseB || location.Title != "Title B" {
		t.Fatalf("Hermes initial location = %#v, want database B", location)
	}

	databaseA := filepath.Join(rootA, "state.db")
	createHermesTestDatabase(t, sqlite, databaseA, sessionID, cwd, "Title A",
		fmt.Sprintf("INSERT INTO messages(session_id,role,content,timestamp,active,compacted) VALUES('%s','assistant','Transcript A',100,1,0);", sessionID))
	page, err := reader.ReadFor("hermes", cwd, sessionID, "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 1 || page.Entries[0].Text != "Transcript B" {
		t.Fatalf("Hermes cached transcript = %#v, want database B content", page)
	}
}
