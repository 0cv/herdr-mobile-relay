package conversation

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenCodeQueryCapsNewestRowsThenRestoresChronologicalOrder(t *testing.T) {
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 is unavailable")
	}
	database := filepath.Join(t.TempDir(), "opencode.db")
	var sql strings.Builder
	sql.WriteString("BEGIN;CREATE TABLE session(id TEXT PRIMARY KEY,directory TEXT,title TEXT,time_updated INTEGER,agent TEXT);CREATE TABLE message(id TEXT PRIMARY KEY,session_id TEXT,time_created INTEGER,data TEXT);CREATE TABLE part(id TEXT PRIMARY KEY,message_id TEXT,data TEXT);INSERT INTO session VALUES('ses_ordering','/work/project','Title',1,'opencode');")
	for index := range maxOpenCodeRows + 10 {
		fmt.Fprintf(&sql, "INSERT INTO message VALUES('m%05d','ses_ordering',%d,'{\"role\":\"user\"}');INSERT INTO part VALUES('p%05d','m%05d','{\"type\":\"text\",\"text\":\"row %d\"}');", index, index, index, index, index)
	}
	sql.WriteString("COMMIT;")
	command := exec.Command(sqlite, database)
	command.Stdin = strings.NewReader(sql.String())
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create OpenCode database: %v: %s", err, output)
	}
	reader := newOpenCodeReader(t.TempDir())
	reader.binary = sqlite
	rows, clipped, code := reader.query(database, "ses_ordering")
	if code != "" {
		t.Fatalf("query code = %q", code)
	}
	if !clipped || len(rows) != maxOpenCodeRows {
		t.Fatalf("query returned %d rows, clipped %v", len(rows), clipped)
	}
	if rows[0].MessageID != "m00010" || rows[len(rows)-1].MessageID != "m05009" {
		t.Fatalf("query range = %q through %q", rows[0].MessageID, rows[len(rows)-1].MessageID)
	}
}

func TestOpenCodeDirectoryBindingResolvesSymlinks(t *testing.T) {
	realDirectory := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(realDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "project-link")
	if err := os.Symlink(realDirectory, link); err != nil {
		t.Fatal(err)
	}
	if !sameOpenCodeDirectory(link, realDirectory) {
		t.Fatal("symlinked workspace did not match its real OpenCode directory")
	}
	other := filepath.Join(t.TempDir(), "other")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if sameOpenCodeDirectory(other, realDirectory) {
		t.Fatal("different workspace matched OpenCode session directory")
	}
}
