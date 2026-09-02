package conversation

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
)

func TestOpenCodeQueryPagesNewestMessagesThenRestoresChronologicalOrder(t *testing.T) {
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 is unavailable")
	}
	database := filepath.Join(t.TempDir(), "opencode.db")
	const messageCount = 5010
	var sql strings.Builder
	sql.WriteString("BEGIN;CREATE TABLE session(id TEXT PRIMARY KEY,directory TEXT,title TEXT,time_updated INTEGER,agent TEXT);CREATE TABLE message(id TEXT PRIMARY KEY,session_id TEXT,time_created INTEGER,data TEXT);CREATE TABLE part(id TEXT PRIMARY KEY,message_id TEXT,data TEXT);INSERT INTO session VALUES('ses_ordering','/work/project','Title',1,'opencode');")
	for index := range messageCount {
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
	rows, hasMore, code := reader.query(database, "ses_ordering", "", maxPageSize)
	if code != "" {
		t.Fatalf("query code = %q", code)
	}
	if !hasMore || len(rows) != maxPageSize {
		t.Fatalf("query returned %d rows, hasMore %v", len(rows), hasMore)
	}
	if rows[0].MessageID != "m04810" || rows[len(rows)-1].MessageID != "m05009" {
		t.Fatalf("query range = %q through %q", rows[0].MessageID, rows[len(rows)-1].MessageID)
	}
	reader.binary = filepath.Join(t.TempDir(), "missing-sqlite")
	cached, cachedMore, code := reader.query(database, "ses_ordering", "", maxPageSize)
	if code != "" || !cachedMore || len(cached) != maxPageSize {
		t.Fatalf("cached query returned %d rows, hasMore %v, code %q", len(cached), cachedMore, code)
	}
	reader.binary = sqlite
	older, olderMore, code := reader.query(database, "ses_ordering", rows[0].MessageID, maxPageSize)
	if code != "" || !olderMore || len(older) != maxPageSize {
		t.Fatalf("older page returned %d rows, more %v, code %q", len(older), olderMore, code)
	}
	if older[0].MessageID != "m04610" || older[len(older)-1].MessageID != "m04809" {
		t.Fatalf("older page range = %q through %q", older[0].MessageID, older[len(older)-1].MessageID)
	}
}

func TestOpenCodeQueryOutputParsesStoredJSONText(t *testing.T) {
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 is unavailable")
	}
	database := filepath.Join(t.TempDir(), "opencode.db")
	sql := `CREATE TABLE session(id TEXT PRIMARY KEY,directory TEXT,title TEXT,time_updated INTEGER,agent TEXT);
CREATE TABLE message(id TEXT PRIMARY KEY,session_id TEXT,time_created INTEGER,data TEXT);
CREATE TABLE part(id TEXT PRIMARY KEY,message_id TEXT,data TEXT);
INSERT INTO session VALUES('ses_parser','/work/project','Title',1,'opencode');
INSERT INTO message VALUES('message-one','ses_parser',2,'{"role":"user"}');
INSERT INTO part VALUES('part-one','message-one','{"type":"text","text":"hello"}');`
	command := exec.Command(sqlite, database)
	command.Stdin = strings.NewReader(sql)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create OpenCode database: %v: %s", err, output)
	}
	reader := newOpenCodeReader(t.TempDir())
	reader.binary = sqlite
	rows, hasMore, code := reader.query(database, "ses_parser", "", defaultPageSize)
	if code != "" || hasMore {
		t.Fatalf("query code = %q, hasMore = %v", code, hasMore)
	}
	entries, corrupt := parseOpenCodeRows(rows)
	if corrupt || len(entries) != 1 || entries[0].Role != "user" || entries[0].Text != "hello" {
		t.Fatalf("parsed entries = %#v, corrupt = %v", entries, corrupt)
	}
}

func TestOpenCodePartialCorruptionIsNotReportedAsFileTruncation(t *testing.T) {
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 is unavailable")
	}
	root := t.TempDir()
	cwd := filepath.Join(root, "project")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(root, "opencode.db")
	sql := fmt.Sprintf(`CREATE TABLE session(id TEXT PRIMARY KEY,directory TEXT,title TEXT,time_updated INTEGER,agent TEXT);
CREATE TABLE message(id TEXT PRIMARY KEY,session_id TEXT,time_created INTEGER,data TEXT);
CREATE TABLE part(id TEXT PRIMARY KEY,message_id TEXT,data TEXT);
INSERT INTO session VALUES('ses_corrupt','%s','Title',1,'opencode');
INSERT INTO message VALUES('message-one','ses_corrupt',2,'{"role":"assistant"}');
INSERT INTO part VALUES('part-one','message-one','{"type":"text","text":"valid turn"}');
INSERT INTO part VALUES('part-two','message-one','not-json');`, cwd)
	command := exec.Command(sqlite, database)
	command.Stdin = strings.NewReader(sql)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create OpenCode database: %v: %s", err, output)
	}
	t.Setenv(agentroots.OpenCodeListEnv, root)
	reader := NewReader(t.TempDir())
	reader.openCode.binary = sqlite
	page, err := reader.readOpenCodeFor(cwd, "ses_corrupt", "", defaultPageSize)
	if err != nil {
		t.Fatal(err)
	}
	if !page.Available || len(page.Entries) != 1 || !page.SourceCorrupt || page.FileTruncated {
		t.Fatalf("partially corrupt OpenCode page = %#v", page)
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

func TestOpenCodeReaderSearchesEveryConfiguredDatabase(t *testing.T) {
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 is unavailable")
	}
	rootA := t.TempDir()
	rootB := t.TempDir()
	createDatabase := func(root, sessionSQL string) {
		t.Helper()
		database := filepath.Join(root, "opencode.db")
		schema := "CREATE TABLE session(id TEXT PRIMARY KEY,directory TEXT,title TEXT,time_updated INTEGER,agent TEXT);" +
			"CREATE TABLE message(id TEXT PRIMARY KEY,session_id TEXT,time_created INTEGER,data TEXT);" +
			"CREATE TABLE part(id TEXT PRIMARY KEY,message_id TEXT,data TEXT);" + sessionSQL
		command := exec.Command(sqlite, database, schema)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("create OpenCode database: %v: %s", err, output)
		}
	}
	createDatabase(rootA, "")
	createDatabase(rootB,
		`INSERT INTO session VALUES('ses_second','/work/project','Second root',2,'opencode');`+
			`INSERT INTO message VALUES('m1','ses_second',1,'{"role":"assistant"}');`+
			`INSERT INTO part VALUES('p1','m1','{"type":"text","text":"found"}');`,
	)
	t.Setenv(agentroots.OpenCodeListEnv, rootA+string(os.PathListSeparator)+rootB)
	reader := newOpenCodeReader(t.TempDir())
	reader.binary = sqlite
	entries, _, _, metadata, code := reader.read("ses_second", "", defaultPageSize)
	if code != "" || len(entries) != 1 || entries[0].Text != "found" || metadata.Title != "Second root" {
		t.Fatalf("second-root result = %#v, metadata %#v, code %q", entries, metadata, code)
	}
}
