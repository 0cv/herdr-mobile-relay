package conversation

import (
	"context"
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
	page, err := reader.readOpenCode("ses_corrupt", "", defaultPageSize)
	if err != nil {
		t.Fatal(err)
	}
	if !page.Available || len(page.Entries) != 1 || !page.SourceCorrupt || page.FileTruncated {
		t.Fatalf("partially corrupt OpenCode page = %#v", page)
	}
}

func TestOpenCodeCorruptRowsStillReturnAContinuationPage(t *testing.T) {
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 is unavailable")
	}
	root := t.TempDir()
	cwd := filepath.Join(root, "work")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(root, "opencode.db")
	sql := fmt.Sprintf(`CREATE TABLE session(id TEXT PRIMARY KEY,directory TEXT,title TEXT,time_updated INTEGER,agent TEXT);
CREATE TABLE message(id TEXT PRIMARY KEY,session_id TEXT,time_created INTEGER,data TEXT);
CREATE TABLE part(id TEXT PRIMARY KEY,message_id TEXT,data TEXT);
INSERT INTO session VALUES('ses_bad1','%s','Bad',1,'opencode');
INSERT INTO message VALUES('m1','ses_bad1',1,'{"role":"unexpected"}');`, cwd)
	command := exec.Command(sqlite, database)
	command.Stdin = strings.NewReader(sql)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create corrupt OpenCode database: %v: %s", err, output)
	}
	t.Setenv(agentroots.OpenCodeListEnv, root)
	reader := NewReader(t.TempDir())
	reader.openCode.binary = sqlite
	page, err := reader.readOpenCode("ses_bad1", "", 1)
	if err != nil || !page.Available || !page.SourceCorrupt || len(page.Entries) != 0 {
		t.Fatalf("corrupt OpenCode page = %#v, err = %v", page, err)
	}
}

// OpenCode resumes a session in its stored directory from any launch
// directory (#61), so the pane cwd must not hide the session Herdr reported.
func TestOpenCodeSessionIsNotBoundToPaneDirectory(t *testing.T) {
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 is unavailable")
	}
	root := t.TempDir()
	sessionDirectory := filepath.Join(root, "Code")
	panes := map[string]string{
		"subdirectory": filepath.Join(sessionDirectory, "helper"),
		"parent":       root,
		"sibling":      filepath.Join(root, "Other"),
		"missing":      filepath.Join(root, "removed"),
	}
	for name, pane := range panes {
		if name == "missing" {
			continue
		}
		if err := os.MkdirAll(pane, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	database := filepath.Join(root, "opencode.db")
	sql := fmt.Sprintf(`CREATE TABLE session(id TEXT PRIMARY KEY,directory TEXT,title TEXT,time_updated INTEGER,agent TEXT);
CREATE TABLE message(id TEXT PRIMARY KEY,session_id TEXT,time_created INTEGER,data TEXT);
CREATE TABLE part(id TEXT PRIMARY KEY,message_id TEXT,data TEXT);
INSERT INTO session VALUES('ses_resumed','%s','Resumed',2,'opencode');
INSERT INTO message VALUES('m1','ses_resumed',1,'{"role":"user"}');
INSERT INTO part VALUES('p1','m1','{"type":"text","text":"older"}');
INSERT INTO message VALUES('m2','ses_resumed',2,'{"role":"assistant"}');
INSERT INTO part VALUES('p2','m2','{"type":"text","text":"newer"}');`, sessionDirectory)
	command := exec.Command(sqlite, database)
	command.Stdin = strings.NewReader(sql)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create OpenCode database: %v: %s", err, output)
	}
	t.Setenv(agentroots.OpenCodeListEnv, root)
	reader := NewReader(t.TempDir())
	reader.openCode.binary = sqlite
	browser, err := NewBrowser(reader, t.TempDir(), DefaultBrowserOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	for name, pane := range panes {
		t.Run(name, func(t *testing.T) {
			page, err := reader.ReadWithProject("opencode", ProjectContext{CWD: pane}, "ses_resumed", "", defaultPageSize)
			if err != nil || !page.Available || len(page.Entries) != 2 || page.Entries[1].Text != "newer" {
				t.Fatalf("pane read = %#v, err = %v", page, err)
			}
			scope := BrowseScope{Provider: "opencode", CWD: pane, SessionID: "ses_resumed"}
			latest, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
			if err != nil || !latest.Available || latest.Mode != BrowseNative || len(latest.Entries) != 1 || latest.Entries[0].Text != "newer" || latest.NextCursor == "" {
				t.Fatalf("browser latest page = %#v, err = %v", latest, err)
			}
			older, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: latest.NextCursor, Limit: 1})
			if err != nil || !older.Available || older.HasMore || len(older.Entries) != 1 || older.Entries[0].Text != "older" {
				t.Fatalf("browser older page = %#v, err = %v", older, err)
			}
		})
	}
	if page, err := reader.ReadWithProject("opencode", ProjectContext{CWD: panes["sibling"]}, "ses_absent", "", defaultPageSize); err != nil || page.Available || page.ReasonCode != "invalid_session" {
		t.Fatalf("unknown session read = %#v, err = %v", page, err)
	}
}

func TestBrowserUsesNativeOpenCodeCursorPaging(t *testing.T) {
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
INSERT INTO session VALUES('ses_browser','%s','Browser',2,'opencode');
INSERT INTO message VALUES('m1','ses_browser',1,'{"role":"user"}');
INSERT INTO part VALUES('p1','m1','{"type":"text","text":"older"}');
INSERT INTO message VALUES('m2','ses_browser',2,'{"role":"assistant"}');
INSERT INTO part VALUES('p2','m2','{"type":"text","text":"newer"}');`, cwd)
	command := exec.Command(sqlite, database)
	command.Stdin = strings.NewReader(sql)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create OpenCode database: %v: %s", err, output)
	}
	t.Setenv(agentroots.OpenCodeListEnv, root)
	reader := NewReader(t.TempDir())
	reader.openCode.binary = sqlite
	browser, err := NewBrowser(reader, t.TempDir(), DefaultBrowserOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "opencode", CWD: cwd, SessionID: "ses_browser"}
	latest, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || latest.Mode != BrowseNative || len(latest.Entries) != 1 || !latest.HasMore || latest.NextCursor == "" || latest.Entries[0].Text != "newer" || latest.SourceRevision == "" {
		t.Fatalf("OpenCode latest page = %#v, err = %v", latest, err)
	}
	appendCommand := exec.Command(sqlite, database, "INSERT INTO message VALUES('m3','ses_browser',3,'{\"role\":\"assistant\"}'); INSERT INTO part VALUES('p3','m3','{\"type\":\"text\",\"text\":\"appended\"}'); UPDATE session SET time_updated=3 WHERE id='ses_browser';")
	if output, err := appendCommand.CombinedOutput(); err != nil {
		t.Fatalf("append OpenCode database: %v: %s", err, output)
	}
	older, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: latest.NextCursor, Limit: 1})
	if err != nil || older.Mode != BrowseNative || older.HasMore || len(older.Entries) != 1 || older.Entries[0].Text != "older" {
		t.Fatalf("OpenCode older page after append = %#v, err = %v", older, err)
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

const (
	openCodeV1Schema = `CREATE TABLE session(id TEXT PRIMARY KEY,directory TEXT,title TEXT,time_updated INTEGER,agent TEXT);
CREATE TABLE message(id TEXT PRIMARY KEY,session_id TEXT,time_created INTEGER,data TEXT);
CREATE TABLE part(id TEXT PRIMARY KEY,message_id TEXT,data TEXT);`
	openCodeV2Schema = `CREATE TABLE session_v2(id TEXT PRIMARY KEY,project_id TEXT NOT NULL DEFAULT 'p',directory TEXT NOT NULL,title TEXT,version TEXT NOT NULL DEFAULT '2',agent TEXT,time_created INTEGER NOT NULL DEFAULT 0,time_updated INTEGER NOT NULL DEFAULT 0);
CREATE TABLE session_message(id TEXT PRIMARY KEY,session_id TEXT NOT NULL,type TEXT NOT NULL,seq INTEGER NOT NULL,time_created INTEGER NOT NULL,time_updated INTEGER NOT NULL DEFAULT 0,data TEXT NOT NULL);
CREATE UNIQUE INDEX session_message_session_seq_idx ON session_message(session_id,seq);`
)

func createOpenCodeFixture(t *testing.T, database, sql string) string {
	t.Helper()
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 is unavailable")
	}
	command := exec.Command(sqlite, database)
	command.Stdin = strings.NewReader(sql)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create OpenCode database: %v: %s", err, output)
	}
	return sqlite
}

func openCodeFixtureReader(t *testing.T, sqlite string, roots ...string) *Reader {
	t.Helper()
	t.Setenv(agentroots.OpenCodeListEnv, strings.Join(roots, string(os.PathListSeparator)))
	reader := NewReader(t.TempDir())
	reader.openCode.binary = sqlite
	return reader
}

func openCodeV2Message(id, session, kind string, seq, created int, data string) string {
	return fmt.Sprintf("INSERT INTO session_message(id,session_id,type,seq,time_created,data) VALUES('%s','%s','%s',%d,%d,'%s');",
		id, session, kind, seq, created, strings.ReplaceAll(data, "'", "''"))
}

func TestOpenCodeV2FreshStoreReadsVisibleTurns(t *testing.T) {
	root := t.TempDir()
	assistant := `{"time":{"created":2},"agent":"build","content":[` +
		`{"type":"reasoning","text":"private thoughts"},` +
		`{"type":"text","text":"first answer"},` +
		`{"type":"tool","id":"call_1","name":"shell","state":{"status":"completed","input":{"command":"go vet ./..."},"content":[{"type":"text","text":"ok"},{"type":"text","text":"done"}],"metadata":{"exit":0}}},` +
		`{"type":"tool","id":"call_2","name":"shell","state":{"status":"error","input":{"command":"false"},"content":[{"type":"text","text":"boom"}]}},` +
		`{"type":"tool","id":"call_3","name":"read","state":{"status":"completed","input":{},"error":{"message":"bad"}}},` +
		`{"type":"mystery","text":"ignored"},` +
		`{"type":"text","text":"second answer"}],"finish":"stop"}`
	sql := openCodeV2Schema + `INSERT INTO session_v2(id,directory,title,agent,time_updated) VALUES('ses_fresh','/work','Fresh','build',9);` +
		openCodeV2Message("m1", "ses_fresh", "user", 1, 1, `{"time":{"created":1},"text":"hello","files":[],"agents":[]}`) +
		openCodeV2Message("m2", "ses_fresh", "synthetic", 2, 2, `{"text":"synthetic"}`) +
		openCodeV2Message("m3", "ses_fresh", "assistant", 3, 3, assistant) +
		openCodeV2Message("m4", "ses_fresh", "idle", 4, 4, `{"outcome":"succeeded"}`) +
		openCodeV2Message("m5", "ses_fresh", "compaction", 5, 5, `{"summary":"x"}`) +
		openCodeV2Message("m6", "ses_fresh", "system", 6, 6, `{"text":"sys"}`)
	sqlite := createOpenCodeFixture(t, filepath.Join(root, "opencode.db"), sql)
	reader := openCodeFixtureReader(t, sqlite, root)
	page, err := reader.readOpenCode("ses_fresh", "", defaultPageSize)
	if err != nil || !page.Available || page.SourceCorrupt || page.HasMore || page.Total != 2 || len(page.Entries) != 2 {
		t.Fatalf("page = %#v, err = %v", page, err)
	}
	user, assistantEntry := page.Entries[0], page.Entries[1]
	if user.Role != "user" || user.Text != "hello" || user.ID != "m1" {
		t.Fatalf("user entry = %#v", user)
	}
	if assistantEntry.Role != "assistant" || assistantEntry.Text != "first answer\nsecond answer" || strings.Contains(assistantEntry.Text, "private") {
		t.Fatalf("assistant entry = %#v", assistantEntry)
	}
	if assistantEntry.Timestamp == "" || len(assistantEntry.Tools) != 3 {
		t.Fatalf("assistant tools = %#v", assistantEntry)
	}
	completed, failed, withError := assistantEntry.Tools[0], assistantEntry.Tools[1], assistantEntry.Tools[2]
	if completed.Name != "shell" || completed.ID != "call_1" || completed.Output != "ok\ndone" || completed.Error || !strings.Contains(completed.Input, "go vet") {
		t.Fatalf("completed tool = %#v", completed)
	}
	if !failed.Error || failed.Output != "boom" {
		t.Fatalf("error tool = %#v", failed)
	}
	if !withError.Error || withError.Name != "read" {
		t.Fatalf("state.error tool = %#v", withError)
	}
}

// The tool items are verbatim from an OpenCode 2.0.29 store written through
// its TUI. A failed command is stored as "completed" with metadata.exit, and
// its exit status is a separate content block.
func TestOpenCodeV2RealToolCallsKeepBlocksApartAndFlagFailedExits(t *testing.T) {
	root := t.TempDir()
	assistant := `{"time":{"created":1792068926669},"agent":"build","content":[` +
		`{"type":"reasoning","text":"run the commands"},` +
		`{"type":"tool","id":"call-b09c","name":"shell","executed":false,"state":{"status":"completed","input":{"command":"wc -l notes.txt"},"content":[{"type":"text","text":"       3 notes.txt\n"}],"metadata":{"status":"completed","truncated":false,"exit":0}},"time":{"created":1792068930629}},` +
		`{"type":"tool","id":"call-c885","name":"shell","executed":false,"state":{"status":"completed","input":{"command":"cat does-not-exist.txt"},"content":[{"type":"text","text":"cat: does-not-exist.txt: No such file or directory\n"},{"type":"text","text":"Exited with code 1"}],"metadata":{"status":"completed","truncated":false,"exit":1}},"time":{"created":1792068930633}},` +
		`{"type":"tool","id":"call-2f1a","name":"shell","executed":false,"state":{"status":"completed","input":{"command":"printf foo; exit 3"},"content":[{"type":"text","text":"foo"},{"type":"text","text":"Exited with code 3"}],"metadata":{"status":"completed","truncated":false,"exit":3}},"time":{"created":1792068990000}},` +
		`{"type":"tool","id":"call-5ab5","name":"read","executed":false,"state":{"status":"completed","input":{"path":"notes.txt"},"content":[{"type":"text","text":"Read file notes.txt, lines 1-3\n1: alpha\n2: beta\n3: gamma"}],"metadata":{"truncated":false}},"time":{"created":1792068930678}}],` +
		`"finish":"tool-calls"}`
	sql := openCodeV2Schema + `INSERT INTO session_v2(id,directory,title,agent,time_updated) VALUES('ses_tools','/work','Tools','build',9);` +
		openCodeV2Message("m1", "ses_tools", "user", 4, 1, `{"time":{"created":1},"text":"run them","files":[],"agents":[]}`) +
		openCodeV2Message("m2", "ses_tools", "assistant", 5, 2, assistant)
	sqlite := createOpenCodeFixture(t, filepath.Join(root, "opencode.db"), sql)
	reader := openCodeFixtureReader(t, sqlite, root)
	page, err := reader.readOpenCode("ses_tools", "", defaultPageSize)
	if err != nil || !page.Available || page.SourceCorrupt || len(page.Entries) != 2 || len(page.Entries[1].Tools) != 4 {
		t.Fatalf("page = %#v, err = %v", page, err)
	}
	want := []struct {
		name, output string
		err          bool
	}{
		{"shell", "3 notes.txt", false},
		{"shell", "cat: does-not-exist.txt: No such file or directory\nExited with code 1", true},
		{"shell", "foo\nExited with code 3", true},
		{"read", "Read file notes.txt, lines 1-3\n1: alpha\n2: beta\n3: gamma", false},
	}
	for index, tool := range page.Entries[1].Tools {
		if tool.Name != want[index].name || tool.Output != want[index].output || tool.Error != want[index].err {
			t.Errorf("tool %d = name %q output %q error %v, want %+v", index, tool.Name, tool.Output, tool.Error, want[index])
		}
	}
}

func TestOpenCodeV1ShellExitCodeMarksToolFailed(t *testing.T) {
	root := t.TempDir()
	sql := openCodeV1Schema + `INSERT INTO session VALUES('ses_exit','/work','Exit',2,'build');` +
		`INSERT INTO message VALUES('m1','ses_exit',1,'{"role":"assistant"}');` +
		`INSERT INTO part VALUES('p1','m1','{"type":"tool","tool":"bash","callID":"c1","state":{"status":"completed","input":{"command":"true"},"output":"","metadata":{"exit":0}}}');` +
		`INSERT INTO part VALUES('p2','m1','{"type":"tool","tool":"bash","callID":"c2","state":{"status":"completed","input":{"command":"false"},"output":"","metadata":{"exit":1}}}');` +
		`INSERT INTO part VALUES('p3','m1','{"type":"tool","tool":"bash","callID":"c3","state":{"status":"completed","input":{"command":"odd"},"output":"x","metadata":{"exit":"unknown"}}}');` +
		`INSERT INTO part VALUES('p4','m1','{"type":"tool","tool":"read","callID":"c4","state":{"status":"completed","input":{},"output":"y","metadata":"not-an-object"}}');`
	sqlite := createOpenCodeFixture(t, filepath.Join(root, "opencode.db"), sql)
	reader := openCodeFixtureReader(t, sqlite, root)
	page, err := reader.readOpenCode("ses_exit", "", defaultPageSize)
	if err != nil || !page.Available || page.SourceCorrupt || len(page.Entries) != 1 || len(page.Entries[0].Tools) != 4 {
		t.Fatalf("page = %#v, err = %v", page, err)
	}
	for index, want := range []bool{false, true, false, false} {
		if tool := page.Entries[0].Tools[index]; tool.Error != want {
			t.Errorf("tool %s error = %v, want %v", tool.ID, tool.Error, want)
		}
	}
}

func TestOpenCodeV2PagingOrdersBySeqAndSkipsHiddenTurns(t *testing.T) {
	root := t.TempDir()
	// Visible turns v1..v5 are interleaved with hidden rows. Equal
	// time_created values prove ordering uses seq, and the ID order is
	// deliberately reversed from seq.
	sql := openCodeV2Schema + `INSERT INTO session_v2(id,directory,title,agent,time_updated) VALUES('ses_pages','/work','Pages','build',9);` +
		openCodeV2Message("z-hidden", "ses_pages", "idle", 1, 5, `{}`) +
		openCodeV2Message("e5", "ses_pages", "user", 2, 5, `{"text":"one"}`) +
		openCodeV2Message("d4", "ses_pages", "idle", 3, 5, `{}`) +
		openCodeV2Message("c3", "ses_pages", "assistant", 4, 5, `{"content":[{"type":"text","text":"two"}]}`) +
		openCodeV2Message("b2", "ses_pages", "synthetic", 5, 5, `{"text":"x"}`) +
		openCodeV2Message("a1", "ses_pages", "user", 6, 6, `{"text":"three"}`) +
		openCodeV2Message("h1", "ses_pages", "idle", 7, 7, `{}`) +
		openCodeV2Message("h2", "ses_pages", "compaction", 8, 8, `{}`) +
		openCodeV2Message("h3", "ses_pages", "system", 9, 8, `{}`) +
		openCodeV2Message("f6", "ses_pages", "assistant", 10, 8, `{"content":[{"type":"text","text":"four"}]}`) +
		openCodeV2Message("g7", "ses_pages", "user", 11, 8, `{"text":"five"}`) +
		openCodeV2Message("h4", "ses_pages", "idle", 12, 9, `{}`)
	sqlite := createOpenCodeFixture(t, filepath.Join(root, "opencode.db"), sql)
	reader := openCodeFixtureReader(t, sqlite, root)
	texts := func(page Page) string {
		var parts []string
		for _, entry := range page.Entries {
			parts = append(parts, entry.Text)
		}
		return strings.Join(parts, ",")
	}
	first, err := reader.readOpenCode("ses_pages", "", 2)
	if err != nil || !first.Available || !first.HasMore || first.Total != 5 || texts(first) != "four,five" {
		t.Fatalf("first page = %#v, err = %v", first, err)
	}
	second, err := reader.readOpenCode("ses_pages", first.Entries[0].ID, 2)
	if err != nil || !second.HasMore || texts(second) != "two,three" {
		t.Fatalf("second page = %#v, err = %v", second, err)
	}
	last, err := reader.readOpenCode("ses_pages", second.Entries[0].ID, 2)
	if err != nil || last.HasMore || texts(last) != "one" {
		t.Fatalf("last page = %#v, err = %v", last, err)
	}
	for _, cursor := range []string{"missing", "h1"} {
		page, err := reader.readOpenCode("ses_pages", cursor, 2)
		if err != nil || page.Available || page.ReasonCode != "invalid_cursor" {
			t.Fatalf("cursor %q page = %#v, err = %v", cursor, page, err)
		}
	}
}

func TestOpenCodeUpgradedStoreSelectsLayoutPerSession(t *testing.T) {
	root := t.TempDir()
	sql := openCodeV1Schema + openCodeV2Schema +
		// A migrated: both copies, with different text.
		`INSERT INTO session VALUES('ses_migrated','/work','A',1,'build');
INSERT INTO message VALUES('old-a','ses_migrated',1,'{"role":"user"}');
INSERT INTO part VALUES('old-p','old-a','{"type":"text","text":"legacy copy"}');
INSERT INTO session_v2(id,directory,title,agent) VALUES('ses_migrated','/work','A','build');` +
		openCodeV2Message("new-a", "ses_migrated", "user", 1, 1, `{"text":"migrated copy"}`) +
		// B unmigrated: 1.x tables only.
		`INSERT INTO session VALUES('ses_unmigrated','/work','B',1,'build');
INSERT INTO message VALUES('b1','ses_unmigrated',1,'{"role":"user"}');
INSERT INTO part VALUES('bp1','b1','{"type":"text","text":"only legacy"}');` +
		// C: empty in session_v2, absent from session.
		`INSERT INTO session_v2(id,directory,title,agent) VALUES('ses_emptyv2','/work','C','build');` +
		// D: migrated session holding only hidden rows must not fall back.
		`INSERT INTO session VALUES('ses_hidden','/work','D',1,'build');
INSERT INTO message VALUES('d-old','ses_hidden',1,'{"role":"user"}');
INSERT INTO part VALUES('dp','d-old','{"type":"text","text":"legacy d"}');
INSERT INTO session_v2(id,directory,title,agent) VALUES('ses_hidden','/work','D','build');` +
		openCodeV2Message("d-idle", "ses_hidden", "idle", 1, 1, `{}`)
	sqlite := createOpenCodeFixture(t, filepath.Join(root, "opencode.db"), sql)
	reader := openCodeFixtureReader(t, sqlite, root)
	page, err := reader.readOpenCode("ses_migrated", "", defaultPageSize)
	if err != nil || !page.Available || len(page.Entries) != 1 || page.Entries[0].Text != "migrated copy" {
		t.Fatalf("migrated page = %#v, err = %v", page, err)
	}
	page, err = reader.readOpenCode("ses_unmigrated", "", defaultPageSize)
	if err != nil || !page.Available || len(page.Entries) != 1 || page.Entries[0].Text != "only legacy" || page.Total != 1 {
		t.Fatalf("unmigrated page = %#v, err = %v", page, err)
	}
	page, err = reader.readOpenCode("ses_emptyv2", "", defaultPageSize)
	if err != nil || !page.Available || len(page.Entries) != 0 || page.Total != 0 || page.ReasonCode != "" {
		t.Fatalf("empty page = %#v, err = %v", page, err)
	}
	page, err = reader.readOpenCode("ses_hidden", "", defaultPageSize)
	if err != nil || !page.Available || len(page.Entries) != 0 {
		t.Fatalf("hidden-only page = %#v, err = %v", page, err)
	}
	page, err = reader.readOpenCode("ses_absent", "", defaultPageSize)
	if err != nil || page.Available || page.ReasonCode != "invalid_session" {
		t.Fatalf("absent page = %#v, err = %v", page, err)
	}
}

func TestOpenCodeV2CorruptRowKeepsOtherEntries(t *testing.T) {
	root := t.TempDir()
	sql := openCodeV2Schema + `INSERT INTO session_v2(id,directory,title,agent) VALUES('ses_badrow','/work','Bad','build');` +
		openCodeV2Message("m1", "ses_badrow", "user", 1, 1, `{"text":"before"}`) +
		openCodeV2Message("m2", "ses_badrow", "assistant", 2, 2, `not-json`) +
		openCodeV2Message("m3", "ses_badrow", "assistant", 3, 3, `{"content":[{"type":"text","text":"after"}]}`)
	sqlite := createOpenCodeFixture(t, filepath.Join(root, "opencode.db"), sql)
	reader := openCodeFixtureReader(t, sqlite, root)
	page, err := reader.readOpenCode("ses_badrow", "", defaultPageSize)
	if err != nil || !page.Available || !page.SourceCorrupt || len(page.Entries) != 2 || page.Entries[0].Text != "before" || page.Entries[1].Text != "after" {
		t.Fatalf("corrupt page = %#v, err = %v", page, err)
	}
}

func TestBrowserUsesNativeOpenCodeV2CursorPaging(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "project")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(root, "opencode.db")
	sql := openCodeV2Schema + `INSERT INTO session_v2(id,directory,title,agent) VALUES('ses_browser2','/elsewhere','Browser','build');` +
		openCodeV2Message("m1", "ses_browser2", "user", 1, 1, `{"text":"older"}`) +
		openCodeV2Message("m2", "ses_browser2", "idle", 2, 2, `{}`) +
		openCodeV2Message("m3", "ses_browser2", "assistant", 3, 3, `{"content":[{"type":"text","text":"newer"}]}`)
	sqlite := createOpenCodeFixture(t, database, sql)
	reader := openCodeFixtureReader(t, sqlite, root)
	browser, err := NewBrowser(reader, t.TempDir(), DefaultBrowserOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	// The pane cwd differs from session_v2.directory (#61).
	scope := BrowseScope{Provider: "opencode", CWD: cwd, SessionID: "ses_browser2"}
	latest, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 1})
	if err != nil || latest.Mode != BrowseNative || len(latest.Entries) != 1 || !latest.HasMore || latest.NextCursor == "" || latest.Entries[0].Text != "newer" || latest.SourceRevision == "" {
		t.Fatalf("latest page = %#v, err = %v", latest, err)
	}
	appendSQL := openCodeV2Message("m4", "ses_browser2", "assistant", 4, 4, `{"content":[{"type":"text","text":"appended"}]}`)
	appendCommand := exec.Command(sqlite, database, appendSQL)
	if output, err := appendCommand.CombinedOutput(); err != nil {
		t.Fatalf("append OpenCode database: %v: %s", err, output)
	}
	older, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: latest.NextCursor, Limit: 1})
	if err != nil || older.Mode != BrowseNative || older.HasMore || len(older.Entries) != 1 || older.Entries[0].Text != "older" {
		t.Fatalf("older page after append = %#v, err = %v", older, err)
	}
	page, err := reader.ReadWithProject("opencode", ProjectContext{CWD: cwd}, "ses_browser2", "", defaultPageSize)
	if err != nil || !page.Available || len(page.Entries) != 3 || page.Entries[2].Text != "appended" {
		t.Fatalf("pane read = %#v, err = %v", page, err)
	}
}

func TestOpenCodeReaderFindsV2SessionInSecondDatabase(t *testing.T) {
	rootA, rootB := t.TempDir(), t.TempDir()
	createOpenCodeFixture(t, filepath.Join(rootA, "opencode.db"), openCodeV1Schema)
	sqlite := createOpenCodeFixture(t, filepath.Join(rootB, "opencode.db"), openCodeV2Schema+
		`INSERT INTO session_v2(id,directory,title,agent) VALUES('ses_second2','/work','Second v2','build');`+
		openCodeV2Message("m1", "ses_second2", "assistant", 1, 1, `{"content":[{"type":"text","text":"found"}]}`))
	openCodeFixtureReader(t, sqlite, rootA, rootB)
	reader := newOpenCodeReader(t.TempDir())
	reader.binary = sqlite
	entries, _, _, metadata, code := reader.read("ses_second2", "", defaultPageSize)
	if code != "" || len(entries) != 1 || entries[0].Text != "found" || metadata.Title != "Second v2" {
		t.Fatalf("second-root result = %#v, metadata %#v, code %q", entries, metadata, code)
	}
}

func TestOpenCodeV1CursorIsInvalidAfterSessionMigrates(t *testing.T) {
	root := t.TempDir()
	database := filepath.Join(root, "opencode.db")
	sql := openCodeV1Schema + openCodeV2Schema +
		`INSERT INTO session VALUES('ses_move','/work','Move',1,'build');
INSERT INTO message VALUES('legacy-1','ses_move',1,'{"role":"user"}');
INSERT INTO part VALUES('p1','legacy-1','{"type":"text","text":"old"}');
INSERT INTO message VALUES('legacy-2','ses_move',2,'{"role":"assistant"}');
INSERT INTO part VALUES('p2','legacy-2','{"type":"text","text":"new"}');`
	sqlite := createOpenCodeFixture(t, database, sql)
	reader := openCodeFixtureReader(t, sqlite, root)
	latest, err := reader.readOpenCode("ses_move", "", 1)
	if err != nil || !latest.Available || !latest.HasMore || len(latest.Entries) != 1 || latest.Entries[0].ID != "legacy-2" {
		t.Fatalf("1.x page = %#v, err = %v", latest, err)
	}
	migrate := `INSERT INTO session_v2(id,directory,title,agent) VALUES('ses_move','/work','Move','build');` +
		openCodeV2Message("v2-1", "ses_move", "user", 1, 1, `{"text":"old"}`) +
		openCodeV2Message("v2-2", "ses_move", "assistant", 2, 2, `{"content":[{"type":"text","text":"new"}]}`)
	if output, err := exec.Command(sqlite, database, migrate).CombinedOutput(); err != nil {
		t.Fatalf("migrate: %v: %s", err, output)
	}
	page, err := reader.readOpenCode("ses_move", latest.Entries[0].ID, 1)
	if err != nil || page.Available || page.ReasonCode != "invalid_cursor" || len(page.Entries) != 0 {
		t.Fatalf("stale cursor page = %#v, err = %v", page, err)
	}
}

func TestOpenCodeUnknownLayoutFails(t *testing.T) {
	root := t.TempDir()
	sqlite := createOpenCodeFixture(t, filepath.Join(root, "opencode.db"), "CREATE TABLE unrelated(id TEXT);")
	reader := openCodeFixtureReader(t, sqlite, root)
	page, err := reader.readOpenCode("ses_unknown", "", defaultPageSize)
	if err != nil || page.Available || page.ReasonCode != "query_failed" {
		t.Fatalf("unknown layout page = %#v, err = %v", page, err)
	}
}
