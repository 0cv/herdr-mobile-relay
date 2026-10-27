package conversation

import (
	"context"
	"errors"

	"github.com/0cv/herdr-mobile-relay/internal/sqliteexec"
)

// runSQLiteJSON executes query through the shared backend and maps errors to
// the conversation package's reason codes.
func runSQLiteJSON(ctx context.Context, exec sqliteexec.Executor, database, query string, maxBytes int) ([]byte, string) {
	if exec == nil || !exec.Ready() {
		return nil, "source_unavailable"
	}
	raw, err := exec.QueryJSON(ctx, database, query, maxBytes)
	if err != nil {
		if errors.Is(err, sqliteexec.ErrOutputLimit) {
			return nil, "output_limit"
		}
		return nil, "query_failed"
	}
	return sqliteexec.NormalizeJSONArray(raw), ""
}

// resolveQueryExecutor picks the SQLite backend for a reader.
//
// When binary is overridden away from the default name "sqlite3" (tests pass an
// absolute LookPath result), prefer that CLI so fixtures keep the pre-native
// behavior. If the override is missing or unusable, fall back to primary
// (usually MustFromEnv / native).
func resolveQueryExecutor(primary sqliteexec.Executor, binary string) sqliteexec.Executor {
	if binary != "" && binary != "sqlite3" {
		if cli, err := sqliteexec.Resolve(sqliteexec.BackendCLI, binary); err == nil {
			return cli
		}
	}
	if primary != nil {
		return primary
	}
	return sqliteexec.MustFromEnv()
}
