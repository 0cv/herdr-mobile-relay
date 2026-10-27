package sqliteexec

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNativeQueryJSONRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	database := filepath.Join(dir, "state.db")

	if err := seedDatabaseWrite(database); err != nil {
		t.Fatal(err)
	}

	exec, err := Resolve(BackendNative, "")
	if err != nil || !exec.Ready() {
		t.Fatalf("native executor: %v ready=%v", err, exec != nil && exec.Ready())
	}
	raw, err := exec.QueryJSON(context.Background(), database,
		`SELECT id AS session_id, title, CAST(n AS REAL) AS timestamp FROM sessions ORDER BY id;`,
		1<<20,
	)
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %#v", rows)
	}
	if rows[0]["session_id"] != "abc" || rows[0]["title"] != "Hello" {
		t.Fatalf("row = %#v", rows[0])
	}
}

func TestNativeReadyWithoutCLI(t *testing.T) {
	t.Parallel()
	exec, err := Resolve(BackendNative, filepath.Join(t.TempDir(), "missing-sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	if !exec.Ready() {
		t.Fatal("native must be ready without sqlite3 CLI")
	}
}

func TestAutoPrefersNative(t *testing.T) {
	t.Parallel()
	exec, err := Resolve(BackendAuto, filepath.Join(t.TempDir(), "missing-sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	if !exec.Ready() {
		t.Fatal("auto must use native when CLI is missing")
	}
	if _, ok := exec.(*nativeExecutor); !ok {
		t.Fatalf("auto executor type = %T, want *nativeExecutor", exec)
	}
}

func TestCLIUnavailable(t *testing.T) {
	t.Parallel()
	if _, err := Resolve(BackendCLI, filepath.Join(t.TempDir(), "missing-sqlite3")); err == nil {
		t.Fatal("expected unavailable")
	}
}

func TestNativeQueryJSONPathWithSpaces(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	database := filepath.Join(dir, "my state.db")
	if err := seedDatabaseWrite(database); err != nil {
		t.Fatal(err)
	}
	exec, err := Resolve(BackendNative, "")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := exec.QueryJSON(context.Background(), database, `SELECT id AS session_id FROM sessions;`, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["session_id"] != "abc" {
		t.Fatalf("rows = %#v", rows)
	}
}

func TestNativeQueryJSONOutputLimit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	database := filepath.Join(dir, "state.db")
	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE sessions(id TEXT); INSERT INTO sessions VALUES('aaaaaaaaaaaaaaaa');`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	exec, err := Resolve(BackendNative, "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = exec.QueryJSON(context.Background(), database, `SELECT id AS session_id FROM sessions;`, 8)
	if !errors.Is(err, ErrOutputLimit) {
		t.Fatalf("err = %v, want ErrOutputLimit", err)
	}
}

func TestNativeQueryJSONDoesNotHTMLEscape(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	database := filepath.Join(dir, "state.db")
	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	// Many '<' characters expand ~6x under encoding/json's default HTML
	// escaping (\u003c). Keep the budget between the unescaped and escaped
	// sizes so only SetEscapeHTML(false) succeeds — matching sqlite3 -json.
	html := strings.Repeat("<div>", 200_000) // 1_200_000 bytes raw
	if _, err := db.Exec(`CREATE TABLE tool(content TEXT); INSERT INTO tool VALUES(?);`, html); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	exec, err := Resolve(BackendNative, "")
	if err != nil {
		t.Fatal(err)
	}
	// Envelope is ["{...}"] — raw HTML fits in ~1.3 MiB; HTML-escaped would be ~7+ MiB.
	maxBytes := 2 << 20
	raw, err := exec.QueryJSON(context.Background(), database, `SELECT content FROM tool;`, maxBytes)
	if err != nil {
		t.Fatalf("native should accept HTML payload under %d bytes: %v", maxBytes, err)
	}
	if !bytes.Contains(raw, []byte("<div>")) {
		t.Fatalf("expected literal <div> in JSON (no \\u003c escaping): %s", raw[:min(80, len(raw))])
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["content"] != html {
		t.Fatalf("round-trip mismatch")
	}
}

func TestNativeQueryJSONHonoursCancellationDuringScan(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	database := filepath.Join(dir, "state.db")
	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE sessions(id TEXT, payload TEXT);`); err != nil {
		t.Fatal(err)
	}
	payload := strings.Repeat("x", 64*1024)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO sessions(id, payload) VALUES(?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 400; i++ {
		if _, err := stmt.Exec(fmt.Sprintf("%d", i), payload); err != nil {
			t.Fatal(err)
		}
	}
	_ = stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	exec, err := Resolve(BackendNative, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already done before QueryJSON — must not succeed
	_, err = exec.QueryJSON(ctx, database, `SELECT id, payload FROM sessions;`, 32<<20)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}

	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = exec.QueryJSON(ctx, database, `SELECT id, payload FROM sessions;`, 32<<20)
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("expected deadline/cancel during large scan")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want deadline or cancel", err)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("cancellation too slow: %s", elapsed)
	}
}

func TestNormalizeJSONArray(t *testing.T) {
	t.Parallel()
	if got := string(NormalizeJSONArray(nil)); got != "[]" {
		t.Fatalf("nil -> %q", got)
	}
	if got := string(NormalizeJSONArray([]byte("  \n"))); got != "[]" {
		t.Fatalf("blank -> %q", got)
	}
	in := []byte(`[{"a":1}]`)
	if got := NormalizeJSONArray(in); string(got) != string(in) {
		t.Fatalf("passthrough = %s", got)
	}
}

func TestReadOnlyDSNEncodesSpaces(t *testing.T) {
	t.Parallel()
	dsn, err := readOnlyDSN("/tmp/my state.db")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dsn, "my%20state.db") {
		t.Fatalf("dsn = %q, want percent-encoded space", dsn)
	}
	if !strings.Contains(dsn, "mode=ro") || !strings.Contains(dsn, "query_only") {
		t.Fatalf("dsn missing read-only options: %q", dsn)
	}
}

func seedDatabaseWrite(database string) error {
	db, err := sql.Open("sqlite", database)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE sessions(id TEXT PRIMARY KEY, title TEXT, n INTEGER); INSERT INTO sessions VALUES('abc','Hello',42);`)
	return err
}
