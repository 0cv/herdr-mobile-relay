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
