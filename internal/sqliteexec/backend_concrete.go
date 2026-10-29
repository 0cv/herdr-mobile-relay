//go:build !herdr_sqlite_logical_test

package sqliteexec

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"

	"github.com/0cv/herdr-mobile-relay/internal/childenv"
	_ "modernc.org/sqlite"
)

// FromEnv builds an executor using HERDR_SQLITE_BACKEND (default auto).
func FromEnv() (Executor, error) {
	return Resolve(ParseBackend(os.Getenv(EnvBackend)), "sqlite3")
}

// Resolve builds an executor for the requested backend.
// cliBinary is the sqlite3 executable name or path used by the CLI backend.
func Resolve(backend Backend, cliBinary string) (Executor, error) {
	if cliBinary == "" {
		cliBinary = "sqlite3"
	}
	switch ParseBackend(string(backend)) {
	case BackendNative:
		return newNative(), nil
	case BackendCLI:
		cli := newCLI(cliBinary)
		if !cli.Ready() {
			return nil, fmt.Errorf("%w: %s not found", ErrUnavailable, cliBinary)
		}
		return cli, nil
	default: // auto
		native := newNative()
		if native.Ready() {
			return native, nil
		}
		cli := newCLI(cliBinary)
		if cli.Ready() {
			return cli, nil
		}
		return nil, ErrUnavailable
	}
}

// MustFromEnv returns FromEnv or a CLI fallback that reports not Ready when
// nothing works. Callers still check Ready/databases().
func MustFromEnv() Executor {
	executor, err := FromEnv()
	if err == nil {
		return executor
	}
	return newCLI("sqlite3")
}

func (c *cliExecutor) Ready() bool {
	_, err := exec.LookPath(c.binary)
	return err == nil
}

func (c *cliExecutor) QueryJSON(ctx context.Context, database, query string, maxBytes int) ([]byte, error) {
	if maxBytes < 1 {
		maxBytes = 1
	}
	command := childenv.CommandContext(ctx, c.binary, "-readonly", "-batch", "-json", database, query)
	stdout := &limitedBuffer{remaining: maxBytes}
	var stderr limitedBuffer
	stderr.remaining = 4096
	command.Stdout = stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if stdout.overflow {
			return nil, ErrOutputLimit
		}
		return nil, fmt.Errorf("sqlite3 query failed: %w", err)
	}
	if stdout.overflow {
		return nil, ErrOutputLimit
	}
	return stdout.Bytes(), nil
}

func (n *nativeExecutor) Ready() bool { return true }

func (n *nativeExecutor) QueryJSON(ctx context.Context, database, query string, maxBytes int) ([]byte, error) {
	if maxBytes < 1 {
		maxBytes = 1
	}
	if err := ctx.Err(); err != nil {
		return nil, err
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
		if err := ctx.Err(); err != nil {
			return nil, err
		}
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
		chunk, err := marshalJSONObject(object)
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	buf.WriteByte(']')
	if buf.Len() > maxBytes {
		return nil, ErrOutputLimit
	}
	return buf.Bytes(), nil
}

func productionInstalledDeps() installedDeps {
	return installedDeps{
		nativeReady: func(context.Context) bool { return true },
		lookup:      exec.LookPath,
		open:        openInstalledDB,
		run: func(ctx context.Context, binary, database, query string, max int) ([]byte, error) {
			return (&cliExecutor{binary: binary}).QueryJSON(ctx, database, query, max)
		},
	}
}
