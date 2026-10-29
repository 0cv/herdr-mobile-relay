package conversation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"

	"github.com/0cv/herdr-mobile-relay/internal/conversationlife"
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

// installedProviderSQL is private construction association, not operation
// authority. Production always retains the concrete executor; logical fixtures
// must explicitly supply the complete callback bundle and use the same core.
// It does not select or resolve an ordinary executor.
type installedProviderSQL struct {
	owner    *conversationlife.Owner
	executor *sqliteexec.InstalledExecutor
	binary   string
	deps     installedProviderSQLDeps
}

type installedProviderSQLDeps struct {
	ready func(*conversationlife.Call) bool
	query func(*conversationlife.Call, string, string, int) ([]byte, error)
}

func bindInstalledProviderSQL(owner *conversationlife.Owner, executor *sqliteexec.InstalledExecutor, binary string) (*installedProviderSQL, error) {
	if owner == nil || executor == nil || !executor.OwnedBy(owner) {
		return nil, conversationlife.ErrRefused
	}
	return &installedProviderSQL{
		owner: owner, executor: executor, binary: binary,
		deps: installedProviderSQLDeps{ready: executor.ReadyIn, query: executor.QueryJSONIn},
	}, nil
}

func (b *installedProviderSQL) valid(binary string) bool {
	return b != nil && b.owner != nil && binary == b.binary &&
		b.deps.ready != nil && b.deps.query != nil &&
		(b.executor == nil || b.executor.OwnedBy(b.owner))
}

// terminalProviderError keeps semantic identities below presentation. SQL has
// already witnessed cleanup errors; never Own/Close or record them again here.
func terminalProviderError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, conversationlife.ErrRefused) || errors.Is(err, sqliteexec.ErrCleanupUncertain) ||
		errors.Is(err, ErrSourceCleanupUncertain)
}

// IsTerminalProviderError reports installed cancellation, admission and cleanup
// failures that must not be downgraded to an ordinary missing transcript.
func IsTerminalProviderError(err error) bool { return terminalProviderError(err) }

// readyIn also counts readiness-only operations. False is not sufficient to
// distinguish an unavailable backend from cancellation during its callback.
func (b *installedProviderSQL) readyIn(parent *conversationlife.Call, binary string) (ready bool, err error) {
	if !b.valid(binary) {
		return false, conversationlife.ErrRefused
	}
	err = b.owner.DoIn(parent, func(call *conversationlife.Call) error {
		ctx := call.Context()
		if err := ctx.Err(); err != nil {
			return err
		}
		ready = b.deps.ready(call)
		return ctx.Err()
	})
	if err != nil {
		return false, err
	}
	return ready, nil
}

// queryIn owns a fresh timeout per query, rather than timing the whole page.
// DoIn validates before observing parent.Context; DoInContext then carries the
// independent query deadline into actual ReadyIn and QueryJSONIn callbacks.
func (b *installedProviderSQL) queryIn(parent *conversationlife.Call, binary, database, query string, maxBytes int, timeout time.Duration) (raw []byte, code string, err error) {
	if !b.valid(binary) {
		return nil, "query_failed", conversationlife.ErrRefused
	}
	err = b.owner.DoIn(parent, func(operation *conversationlife.Call) error {
		if err := operation.Context().Err(); err != nil {
			return err
		}
		// Keep the query's timer independent of the earlier caller deadline.
		// DoInContext bridges the live parent and preserves its exact cause;
		// a derived timeout from that parent can otherwise inherit a generic
		// Canceled result before the parent's DeadlineExceeded is observed.
		queryCtx, cancel := context.WithTimeout(context.WithoutCancel(operation.Context()), timeout)
		defer cancel()
		return b.owner.DoInContext(queryCtx, operation, func(call *conversationlife.Call) error {
			ctx := call.Context()
			if err := ctx.Err(); err != nil {
				return err
			}
			ready := b.deps.ready(call)
			if err := ctx.Err(); err != nil {
				return err
			}
			if !ready {
				code = "source_unavailable"
				return nil
			}
			data, queryErr := b.deps.query(call, database, query, maxBytes)
			queryErr = errors.Join(queryErr, ctx.Err())
			if queryErr != nil {
				code = "query_failed"
				if !terminalProviderError(queryErr) && errors.Is(queryErr, sqliteexec.ErrOutputLimit) {
					code = "output_limit"
				}
				return queryErr
			}
			raw = sqliteexec.NormalizeJSONArray(data)
			return ctx.Err()
		})
	})
	if err != nil {
		if code == "" {
			code = "query_failed"
		}
		return nil, code, err
	}
	return raw, code, nil
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

// Complete metadata-only dependencies. No missing fixture member inherits an
// OS callback. Containment reuses the synchronous source metadata algorithm.
type installedProviderMetadata struct {
	roots     func(context.Context, string) ([]string, error)
	databases func(context.Context, string) ([]string, error)
	eval      func(string) (string, error)
	stat      func(string) (os.FileInfo, error)
}

func productionOpenCodeMetadata() installedProviderMetadata {
	return installedProviderMetadata{agentroots.OpenCodeDataContext, agentroots.OpenCodeDBsContext, filepath.EvalSymlinks, os.Stat}
}

func (d installedProviderMetadata) complete() bool {
	return d.roots != nil && d.databases != nil && d.eval != nil && d.stat != nil
}

func (d installedProviderMetadata) contained(ctx context.Context, path, root string) (string, error) {
	if !d.complete() {
		return "", conversationlife.ErrRefused
	}
	return (installedSourceDeps{eval: d.eval, stat: d.stat}).contained(ctx, path, root)
}

func (d installedProviderMetadata) stamp(ctx context.Context, database string) (openCodeFileStamp, bool, error) {
	if !d.complete() {
		return openCodeFileStamp{}, false, conversationlife.ErrRefused
	}
	var info os.FileInfo
	err := sourceEffect(ctx, func() (err error) { info, err = d.stat(database); return err })
	if err != nil {
		if terminalProviderError(err) {
			return openCodeFileStamp{}, false, err
		}
		return openCodeFileStamp{}, false, nil
	}
	if info == nil {
		return openCodeFileStamp{}, false, conversationlife.ErrRefused
	}
	stamp := openCodeFileStamp{databaseSize: info.Size(), databaseTime: info.ModTime().UnixNano()}
	err = sourceEffect(ctx, func() (err error) { info, err = d.stat(database + "-wal"); return err })
	if terminalProviderError(err) {
		return openCodeFileStamp{}, false, err
	}
	if err == nil {
		if info == nil {
			return openCodeFileStamp{}, false, conversationlife.ErrRefused
		}
		stamp.walSize, stamp.walTime = info.Size(), info.ModTime().UnixNano()
	}
	return stamp, true, ctx.Err()
}
