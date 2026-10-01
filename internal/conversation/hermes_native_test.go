package conversation

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
	"github.com/0cv/herdr-mobile-relay/internal/sqliteexec"

	_ "modernc.org/sqlite"
)

func TestHermesReadUsesNativeBackendWithoutCLI(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "workspace")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(root, "state.db")
	const sessionID = "20260927_123456_abcdef"
	if err := seedHermesNativeDB(database, sessionID, cwd); err != nil {
		t.Fatal(err)
	}

	t.Setenv(agentroots.HermesListEnv, root)
	t.Setenv("HERMES_HOME", "")

	native, err := sqliteexec.Resolve(sqliteexec.BackendNative, filepath.Join(t.TempDir(), "missing-sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	reader := NewReader(t.TempDir())
	reader.hermes.binary = filepath.Join(t.TempDir(), "missing-sqlite3")
	reader.hermes.executor = native

	page, err := reader.ReadFor("hermes", cwd, sessionID, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !page.Available {
		t.Fatalf("page unavailable: %s %s", page.ReasonCode, page.Reason)
	}
	if len(page.Entries) != 2 {
		t.Fatalf("entries = %#v", page.Entries)
	}
	if page.Entries[0].Role != "user" || page.Entries[0].Text != "hello" {
		t.Fatalf("first entry = %#v", page.Entries[0])
	}
	if page.Entries[1].Role != "assistant" || page.Entries[1].Text != "world" {
		t.Fatalf("second entry = %#v", page.Entries[1])
	}
}

func seedHermesNativeDB(database, sessionID, cwd string) error {
	db, err := sql.Open("sqlite", database)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(`
CREATE TABLE sessions(
  id TEXT PRIMARY KEY,
  cwd TEXT,
  title TEXT
);
CREATE TABLE messages(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL,
  role TEXT NOT NULL,
  content TEXT,
  tool_call_id TEXT,
  tool_calls TEXT,
  tool_name TEXT,
  timestamp REAL,
  active INTEGER NOT NULL DEFAULT 1,
  compacted INTEGER NOT NULL DEFAULT 0,
  display_kind TEXT
);
INSERT INTO sessions(id, cwd, title) VALUES(?, ?, 'Native');
INSERT INTO messages(session_id, role, content, timestamp, active, compacted)
VALUES(?, 'user', 'hello', 1, 1, 0);
INSERT INTO messages(session_id, role, content, timestamp, active, compacted)
VALUES(?, 'assistant', 'world', 2, 1, 0);
`, sessionID, cwd, sessionID, sessionID)
	return err
}
