package sqliteexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
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
