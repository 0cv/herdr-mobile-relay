package sqliteexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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

	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query sqlite: %w", err)
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("columns: %w", err)
	}

	var objects []map[string]any
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
		objects = append(objects, object)
		encoded, err := json.Marshal(objects)
		if err != nil {
			return nil, fmt.Errorf("encode: %w", err)
		}
		if len(encoded) > maxBytes {
			return nil, ErrOutputLimit
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows: %w", err)
	}
	if objects == nil {
		return []byte("[]"), nil
	}
	encoded, err := json.Marshal(objects)
	if err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	if len(encoded) > maxBytes {
		return nil, ErrOutputLimit
	}
	return encoded, nil
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
	// mode=ro refuses create; query_only blocks writes if the open succeeds.
	return "file:" + filepath.ToSlash(path) + "?mode=ro&_pragma=query_only(1)", nil
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
