package sqliteexec

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

type nativeExecutor struct{}

func newNative() *nativeExecutor {
	return &nativeExecutor{}
}

func (n *nativeExecutor) Ready() bool { return true }

func (n *nativeExecutor) QueryJSON(ctx context.Context, database, query string, maxBytes int) ([]byte, error) {
	if maxBytes < 1 {
		maxBytes = 1
	}
	dsn, err := readOnlyDSN(database)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query sqlite: %w", err)
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("columns: %w", err)
	}

	var buf bytes.Buffer
	buf.WriteByte('[')
	rowCount := 0
	for rows.Next() {
		holders := make([]any, len(columns))
		ptrs := make([]any, len(columns))
		for i := range holders {
			ptrs[i] = &holders[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		object := make(map[string]any, len(columns))
		for i, name := range columns {
			object[name] = normalizeJSONValue(holders[i])
		}
		chunk, err := json.Marshal(object)
		if err != nil {
			return nil, fmt.Errorf("encode: %w", err)
		}
		need := len(chunk)
		if rowCount > 0 {
			need++ // comma
		}
		if buf.Len()+need+1 > maxBytes { // +1 for closing ']'
			return nil, ErrOutputLimit
		}
		if rowCount > 0 {
			buf.WriteByte(',')
		}
		buf.Write(chunk)
		rowCount++
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows: %w", err)
	}
	buf.WriteByte(']')
	if buf.Len() > maxBytes {
		return nil, ErrOutputLimit
	}
	return buf.Bytes(), nil
}

func readOnlyDSN(database string) (string, error) {
	path := filepath.Clean(strings.TrimSpace(database))
	if path == "" || path == "." {
		return "", fmt.Errorf("database path required")
	}
	if !filepath.IsAbs(path) {
		abs, err := filepath.Abs(path)
		if err != nil {
			return "", err
		}
		path = abs
	}
	// url.URL encodes spaces and reserved characters; scheme file + absolute
	// Path yields file:///... which modernc accepts for read-only opens.
	u := url.URL{
		Scheme:   "file",
		Path:     filepath.ToSlash(path),
		RawQuery: "mode=ro&_pragma=query_only(1)",
	}
	return u.String(), nil
}

func normalizeJSONValue(value any) any {
	switch v := value.(type) {
	case nil:
		return nil
	case []byte:
		return string(v)
	default:
		return v
	}
}

// NormalizeJSONArray turns empty CLI stdout into a JSON array so callers can
// always json.Unmarshal into a slice.
func NormalizeJSONArray(raw []byte) []byte {
	if len(bytes.TrimSpace(raw)) == 0 {
		return []byte("[]")
	}
	return raw
}
