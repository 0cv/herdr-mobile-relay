package sqliteexec

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/0cv/herdr-mobile-relay/internal/conversationlife"
)

type installedRows interface {
	Columns() ([]string, error)
	Next() bool
	Scan(...any) error
	Err() error
	Close() error
}
type installedDB interface {
	SetMaxOpenConns(int)
	PingContext(context.Context) error
	QueryContext(context.Context, string) (installedRows, error)
	Close() error
}
type installedNativeDB struct{ *sql.DB }

func (d installedNativeDB) QueryContext(ctx context.Context, query string) (installedRows, error) {
	rows, err := d.DB.QueryContext(ctx, query)
	if rows == nil {
		return nil, err
	}
	return rows, err
}
func openInstalledDB(dsn string) (installedDB, error) {
	db, err := sql.Open("sqlite", dsn)
	if db == nil {
		return nil, err
	}
	return installedNativeDB{db}, err
}

func (e *InstalledExecutor) queryNative(ctx context.Context, database, query string, maxBytes int) (data []byte, primary error) {
	dsn, err := readOnlyDSN(database)
	if err != nil {
		return nil, err
	}
	var dbSlot, rowsSlot *conversationlife.CloseSlot
	// Slots are installed immediately even for partial handle+error returns.
	// Both closes run independently before the operation pin can be released.
	// Own cannot refuse here: admission validated the immutable constructed owner,
	// and method values of these nonnil acquired handles are nonnil closers.
	defer func() {
		uncertain := false
		if rowsSlot != nil {
			uncertain = rowsSlot.Close() != nil
		}
		if dbSlot != nil {
			uncertain = (dbSlot.Close() != nil) || uncertain
		}
		if uncertain {
			data = nil
			primary = errors.Join(primary, ErrCleanupUncertain)
		}
	}()
	db, err := e.deps.open(dsn)
	if db != nil {
		dbSlot, _ = e.owner.Own(db.Close)
	}
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if db == nil {
		return nil, ErrUnavailable
	}
	db.SetMaxOpenConns(1)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, query)
	if rows != nil {
		rowsSlot, _ = e.owner.Own(rows.Close)
	}
	if err != nil {
		return nil, fmt.Errorf("query sqlite: %w", err)
	}
	if rows == nil {
		return nil, ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	columns, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("columns: %w", err)
	}
	var buf bytes.Buffer
	buf.WriteByte('[')
	count := 0
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
		if count > 0 {
			need++
		}
		// Subtraction avoids overflowing an int for a maximal caller budget.
		if need > maxBytes-buf.Len()-1 {
			return nil, ErrOutputLimit
		}
		if count > 0 {
			buf.WriteByte(',')
		}
		buf.Write(chunk)
		count++
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
