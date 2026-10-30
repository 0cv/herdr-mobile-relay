package conversation

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
	"github.com/0cv/herdr-mobile-relay/internal/conversationlife"
	"github.com/0cv/herdr-mobile-relay/internal/sqliteexec"
)

const (
	hermesQueryTimeout = 10 * time.Second
	maxHermesOutput    = 8 * 1024 * 1024
)

type hermesReader struct {
	home      string
	binary    string
	executor  sqliteexec.Executor
	installed *installedProviderSQL
	metadata  installedProviderMetadata
	// Returning observation only; configured before use, nil in production.
	observe func(context.Context, string)
}

type hermesRow struct {
	SchemaVersion int     `json:"schema_version"`
	SessionID     string  `json:"session_id"`
	CWD           string  `json:"cwd"`
	Title         string  `json:"title"`
	MessageID     int64   `json:"message_id"`
	Role          string  `json:"role"`
	ContentHex    string  `json:"content_hex"`
	ToolCallID    string  `json:"tool_call_id"`
	ToolCalls     string  `json:"tool_calls"`
	ToolName      string  `json:"tool_name"`
	DisplayKind   string  `json:"display_kind"`
	Timestamp     float64 `json:"timestamp"`
	Total         int     `json:"message_total"`
	CursorFound   int     `json:"cursor_found"`
}

type hermesToolRow struct {
	ToolCallID string `json:"tool_call_id"`
	ToolName   string `json:"tool_name"`
	ContentHex string `json:"content_hex"`
}

func newHermesReader(home string) *hermesReader {
	return &hermesReader{home: home, binary: "sqlite3", executor: sqliteexec.MustFromEnv()}
}

func (r *hermesReader) queryExec() sqliteexec.Executor {
	if r.installed != nil {
		return nil
	}
	return resolveQueryExecutor(r.executor, r.binary)
}

func (r *hermesReader) databases() ([]string, string) {
	if r.installed != nil {
		var paths []string
		var code string
		err := r.root(context.Background(), func(c *conversationlife.Call) error {
			var err error
			paths, code, err = r.databasesIn(c)
			return err
		})
		if err != nil {
			return nil, "source_unavailable"
		}
		return paths, code
	}
	if exec := r.queryExec(); exec == nil || !exec.Ready() {
		return nil, "source_unavailable"
	}
	roots := agentroots.HermesData(r.home)
	databases := make([]string, 0, len(roots))
	for index, candidate := range agentroots.HermesDBs(r.home) {
		if index >= len(roots) {
			break
		}
		if path := containedRegularFile(candidate, roots[index]); path != "" {
			databases = append(databases, path)
		}
	}
	if len(databases) == 0 {
		return nil, "source_unavailable"
	}
	return databases, ""
}

func (r *hermesReader) locate(cwd, sessionID string) Location {
	location, _ := r.locateContext(context.Background(), cwd, sessionID)
	return location
}

func (r *hermesReader) locateContext(ctx context.Context, cwd, sessionID string) (Location, string) {
	if r.installed != nil {
		var location Location
		var code string
		err := r.root(ctx, func(c *conversationlife.Call) error {
			var err error
			location, code, err = r.locateIn(c, cwd, sessionID)
			return err
		})
		if err != nil {
			return Location{}, hermesFailureCode(code, err)
		}
		return location, code
	}
	if !safeSessionID(sessionID) {
		return Location{}, "invalid_session"
	}
	databases, code := r.databases()
	if code != "" {
		return Location{}, code
	}
	firstFailure := ""
	for _, database := range databases {
		rows, _, queryCode := r.queryContext(ctx, database, sessionID, "", 1)
		if queryCode != "" {
			if firstFailure == "" {
				firstFailure = queryCode
			}
			continue
		}
		if len(rows) == 0 || rows[0].SessionID != sessionID {
			continue
		}
		if cwd != "" && rows[0].CWD != "" && !sameHermesDirectory(cwd, rows[0].CWD) {
			return Location{}, "invalid_session"
		}
		return Location{Path: database, Root: filepath.Dir(database), Title: rows[0].Title}, ""
	}
	if firstFailure != "" {
		return Location{}, firstFailure
	}
	return Location{}, "invalid_session"
}

func sameHermesDirectory(cwd, directory string) bool {
	realCwd, cwdErr := filepath.EvalSymlinks(filepath.Clean(cwd))
	realDirectory, directoryErr := filepath.EvalSymlinks(filepath.Clean(directory))
	return cwdErr == nil && directoryErr == nil && realCwd == realDirectory
}

func (r *hermesReader) readFrom(database, sessionID, before string, limit int) ([]Entry, bool, bool, hermesRow, string) {
	return r.readFromContext(context.Background(), database, sessionID, before, limit)
}

func (r *hermesReader) readFromContext(ctx context.Context, database, sessionID, before string, limit int) ([]Entry, bool, bool, hermesRow, string) {
	if r.installed != nil {
		var entries []Entry
		var more, corrupt bool
		var metadata hermesRow
		var code string
		err := r.root(ctx, func(c *conversationlife.Call) error {
			var err error
			entries, more, corrupt, metadata, code, err = r.readFromIn(c, database, sessionID, before, limit)
			return err
		})
		if err != nil {
			return nil, false, false, hermesRow{}, hermesFailureCode(code, err)
		}
		return entries, more, corrupt, metadata, code
	}
	if !safeSessionID(sessionID) {
		return nil, false, false, hermesRow{}, "invalid_session"
	}
	if before != "" {
		cursor, err := strconv.ParseInt(before, 10, 64)
		if err != nil || cursor <= 0 {
			return nil, false, false, hermesRow{}, "invalid_cursor"
		}
	}
	rows, hasMore, queryCode := r.queryContext(ctx, database, sessionID, before, limit)
	if queryCode != "" {
		return nil, false, false, hermesRow{}, queryCode
	}
	if len(rows) == 0 || rows[0].SessionID != sessionID {
		return nil, false, false, hermesRow{}, "invalid_session"
	}
	if before != "" && rows[0].CursorFound == 0 {
		return nil, false, false, hermesRow{}, "invalid_cursor"
	}
	entries, corrupt := parseHermesRows(rows)
	if toolCorrupt := r.attachToolResultsContext(ctx, database, sessionID, entries); toolCorrupt {
		corrupt = true
	}
	return entries, hasMore, corrupt, rows[0], ""
}

func (r *hermesReader) query(database, sessionID, before string, limit int) ([]hermesRow, bool, string) {
	return r.queryContext(context.Background(), database, sessionID, before, limit)
}

func (r *hermesReader) queryContext(ctx context.Context, database, sessionID, before string, limit int) ([]hermesRow, bool, string) {
	if r.installed != nil {
		var rows []hermesRow
		var more bool
		var code string
		err := r.root(ctx, func(c *conversationlife.Call) error {
			var err error
			rows, more, code, err = r.queryIn(c, database, sessionID, before, limit)
			return err
		})
		if err != nil {
			return nil, false, hermesFailureCode(code, err)
		}
		return rows, more, code
	}
	query, limit, code := hermesMessageSQL(sessionID, before, limit)
	if code != "" {
		return nil, false, code
	}
	queryCtx, cancel := context.WithTimeout(ctx, hermesQueryTimeout)
	defer cancel()
	raw, code := runSQLiteJSON(queryCtx, r.queryExec(), database, query, maxHermesOutput)
	if code != "" {
		return nil, false, code
	}
	rows, more, code, _ := decodeHermesRows(context.Background(), raw, limit)
	return rows, more, code
}

func hermesMessageSQL(sessionID, before string, limit int) (string, int, string) {
	if limit < 1 {
		limit = defaultPageSize
	}
	if limit > maxPageSize {
		limit = maxPageSize
	}
	sessionHex := hex.EncodeToString([]byte(sessionID))
	cursorCTE := "cursor AS (SELECT NULL AS logical_id WHERE 0)"
	cursorFilter := ""
	cursorFound := "1"
	if before != "" {
		cursor, err := strconv.ParseInt(before, 10, 64)
		if err != nil || cursor <= 0 {
			return "", limit, "invalid_cursor"
		}
		cursorCTE = fmt.Sprintf(
			"cursor AS (SELECT logical_id FROM visible WHERE id=%d AND role IN ('user','assistant') LIMIT 1)",
			cursor,
		)
		cursorFilter = " AND EXISTS(SELECT 1 FROM cursor) AND " +
			"d.logical_id < (SELECT logical_id FROM cursor)"
		cursorFound = "EXISTS(SELECT 1 FROM cursor)"
	}
	query := fmt.Sprintf(
		`WITH raw AS (`+
			`SELECT m.id,m.role,m.active,m.compacted,COALESCE(m.content,'') AS content,`+
			`COALESCE(m.tool_call_id,'') AS tool_call_id,COALESCE(m.tool_calls,'') AS tool_calls,`+
			`COALESCE(m.tool_name,'') AS tool_name,COALESCE(m.display_kind,'') AS display_kind,`+
			`COALESCE(m.timestamp,0) AS logical_timestamp `+
			`FROM messages AS m `+
			`WHERE m.session_id=CAST(X'%s' AS TEXT) AND (m.active=1 OR m.compacted=1) `+
			`AND COALESCE(m.display_kind,'') <> 'hidden'`+
			`), visible AS (`+
			`SELECT raw.*,`+
			`MIN(id) OVER (`+
			`PARTITION BY role,content,tool_call_id,tool_calls,tool_name,logical_timestamp`+
			`) AS logical_id,`+
			`ROW_NUMBER() OVER (`+
			`PARTITION BY role,content,tool_call_id,tool_calls,tool_name,logical_timestamp `+
			`ORDER BY active DESC,id DESC`+
			`) AS generation `+
			`FROM raw`+
			`), displayed AS (`+
			`SELECT id,logical_id,role,content,tool_call_id,tool_calls,tool_name,display_kind,logical_timestamp `+
			`FROM visible WHERE generation=1 AND role IN ('user','assistant')`+
			`), `+cursorCTE+`, selected AS (`+
			`SELECT id,logical_id,role,content,tool_call_id,tool_calls,tool_name,display_kind,logical_timestamp `+
			`FROM displayed AS d WHERE 1=1`+cursorFilter+
			` ORDER BY logical_id DESC LIMIT %d`+
			`) `+
			`SELECT (SELECT schema_version FROM pragma_schema_version) AS schema_version,s.id AS session_id,COALESCE(s.cwd,'') AS cwd,COALESCE(s.title,'') AS title,`+
			`COALESCE(sm.logical_id,0) AS message_id,COALESCE(sm.role,'') AS role,`+
			`hex(COALESCE(sm.content,'')) AS content_hex,COALESCE(sm.tool_call_id,'') AS tool_call_id,`+
			`COALESCE(sm.tool_calls,'') AS tool_calls,COALESCE(sm.tool_name,'') AS tool_name,`+
			`COALESCE(sm.display_kind,'') AS display_kind,COALESCE(sm.logical_timestamp,0) AS timestamp,`+
			`(SELECT COUNT(*) FROM displayed) AS message_total,%s AS cursor_found `+
			`FROM sessions AS s LEFT JOIN selected AS sm ON 1=1 `+
			`WHERE s.id=CAST(X'%s' AS TEXT) ORDER BY sm.logical_id DESC;`,
		sessionHex, limit+1, cursorFound, sessionHex,
	)
	return query, limit, ""
}

func decodeHermesRows(ctx context.Context, raw []byte, limit int) ([]hermesRow, bool, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, "query_failed", err
	}
	var rows []hermesRow
	decodeErr := json.Unmarshal(raw, &rows)
	if err := ctx.Err(); err != nil {
		return nil, false, "query_failed", err
	}
	if decodeErr != nil {
		return nil, false, "source_corrupt", decodeErr
	}
	anchorCount := 0
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return nil, false, "query_failed", err
		}
		if row.MessageID > 0 {
			anchorCount++
		}
	}
	hasMore := anchorCount > limit
	if hasMore && len(rows) > 0 {
		rows = rows[:len(rows)-1]
	}
	for left, right := 0, len(rows)-1; left < right; left, right = left+1, right-1 {
		if err := ctx.Err(); err != nil {
			return nil, false, "query_failed", err
		}
		rows[left], rows[right] = rows[right], rows[left]
	}
	if err := ctx.Err(); err != nil {
		return nil, false, "query_failed", err
	}
	return rows, hasMore, "", nil
}

func (r *hermesReader) attachToolResults(database, sessionID string, entries []Entry) bool {
	return r.attachToolResultsContext(context.Background(), database, sessionID, entries)
}

func (r *hermesReader) attachToolResultsContext(ctx context.Context, database, sessionID string, entries []Entry) bool {
	if r.installed != nil {
		corrupt := false
		// The bounded copy commits after the last live-root check. Later
		// cancellation does not roll back a completed publication.
		err := r.root(ctx, func(c *conversationlife.Call) error {
			candidate, damaged, err := r.attachToolResultsIn(c, database, sessionID, entries)
			if err != nil {
				return err
			}
			if err := c.Context().Err(); err != nil {
				return err
			}
			copy(entries, candidate)
			corrupt = damaged
			return nil
		})
		return err != nil || corrupt
	}
	corrupt, _ := attachHermesTools(context.Background(), entries, func(ids []string) ([]hermesToolRow, string, error) {
		rows, code := r.queryToolRowsContext(ctx, database, sessionID, ids)
		return rows, code, nil
	})
	return corrupt
}

// Shared association/merge algorithm; ordinary parsing remains non-cancellable.
func attachHermesTools(ctx context.Context, entries []Entry, query func([]string) ([]hermesToolRow, string, error)) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	callIDs := make(map[string]struct{})
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		for _, tool := range entry.Tools {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			if id := toolAssociationID(tool); id != "" {
				callIDs[id] = struct{}{}
			}
		}
	}
	if len(callIDs) == 0 {
		return false, ctx.Err()
	}
	ids := make([]string, 0, len(callIDs))
	for id := range callIDs {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		ids = append(ids, id)
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	rows, code, queryErr := query(ids)
	if err := errors.Join(queryErr, ctx.Err()); terminalProviderError(err) {
		return false, err
	}
	if code != "" || queryErr != nil {
		return true, nil
	}
	return attachHermesToolRowsContext(ctx, entries, rows)
}

func attachHermesToolRows(entries []Entry, rows []hermesToolRow) bool {
	corrupt, _ := attachHermesToolRowsContext(context.Background(), entries, rows)
	return corrupt
}

func attachHermesToolRowsContext(ctx context.Context, entries []Entry, rows []hermesToolRow) (bool, error) {
	corrupt := false
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		row.ToolCallID = strings.TrimSpace(row.ToolCallID)
		outputText, outputCorrupt, err := hermesTextHexValueContext(ctx, row.ContentHex)
		if err != nil {
			return false, err
		}
		if outputCorrupt {
			corrupt = true
			continue
		}
		output, truncated := clampText(sanitizeText(outputText), maxEntryBytes)
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if output == "" {
			continue
		}
		for entryIndex := range entries {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			for toolIndex := range entries[entryIndex].Tools {
				if err := ctx.Err(); err != nil {
					return false, err
				}
				tool := &entries[entryIndex].Tools[toolIndex]
				if toolAssociationID(*tool) != row.ToolCallID {
					continue
				}
				tool.State = "completed"
				if tool.Output != "" && tool.Output != output {
					tool.Output += "\n" + output
					tool.Output, tool.Truncated = clampText(tool.Output, maxEntryBytes)
				} else if tool.Output == "" {
					tool.Output = output
					tool.Truncated = truncated
				}
			}
		}
	}
	return corrupt, ctx.Err()
}

func (r *hermesReader) queryToolRows(database, sessionID string, callIDs []string) ([]hermesToolRow, string) {
	return r.queryToolRowsContext(context.Background(), database, sessionID, callIDs)
}

func (r *hermesReader) queryToolRowsContext(ctx context.Context, database, sessionID string, callIDs []string) ([]hermesToolRow, string) {
	if r.installed != nil {
		var rows []hermesToolRow
		var code string
		err := r.root(ctx, func(c *conversationlife.Call) error {
			var err error
			rows, code, err = r.queryToolRowsIn(c, database, sessionID, callIDs)
			return err
		})
		if err != nil {
			return nil, hermesFailureCode(code, err)
		}
		return rows, code
	}
	query, _ := hermesToolSQL(context.Background(), sessionID, callIDs)
	queryCtx, cancel := context.WithTimeout(ctx, hermesQueryTimeout)
	defer cancel()
	raw, code := runSQLiteJSON(queryCtx, r.queryExec(), database, query, maxHermesOutput)
	if code != "" {
		return nil, code
	}
	rows, code, _ := decodeHermesToolRows(context.Background(), raw)
	return rows, code
}

func hermesToolSQL(ctx context.Context, sessionID string, callIDs []string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	sessionHex := hex.EncodeToString([]byte(sessionID))
	encodedIDs := make([]string, 0, len(callIDs))
	for _, id := range callIDs {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		encodedIDs = append(encodedIDs, fmt.Sprintf("CAST(X'%s' AS TEXT)", hex.EncodeToString([]byte(id))))
	}
	query := fmt.Sprintf(
		`SELECT COALESCE(m.tool_call_id,'') AS tool_call_id,COALESCE(m.tool_name,'') AS tool_name,`+
			`hex(COALESCE(m.content,'')) AS content_hex FROM messages AS m `+
			`WHERE m.session_id=CAST(X'%s' AS TEXT) AND m.role='tool' `+
			`AND (m.active=1 OR m.compacted=1) AND COALESCE(m.display_kind,'') <> 'hidden' `+
			`AND m.tool_call_id IN (%s) ORDER BY m.id;`,
		sessionHex, strings.Join(encodedIDs, ","),
	)
	return query, ctx.Err()
}

// Concrete association is checked before admission or any metadata callback.
func newInstalledHermesReaderIn(owner *conversationlife.Owner, parent *conversationlife.Call, home string, executor *sqliteexec.InstalledExecutor) (*hermesReader, error) {
	binding, err := bindInstalledProviderSQL(owner, executor, "sqlite3")
	if err != nil {
		return nil, err
	}
	return constructInstalledHermesIn(parent, home, binding, productionHermesMetadata())
}

func productionHermesMetadata() installedProviderMetadata {
	return installedProviderMetadata{agentroots.HermesDataContext, agentroots.HermesDBsContext, filepath.EvalSymlinks, os.Stat}
}

func constructInstalledHermesIn(parent *conversationlife.Call, home string, binding *installedProviderSQL, metadata installedProviderMetadata) (*hermesReader, error) {
	if !binding.valid("sqlite3") || !metadata.complete() {
		return nil, conversationlife.ErrRefused
	}
	var result *hermesReader
	err := binding.owner.DoIn(parent, func(c *conversationlife.Call) error {
		if err := c.Context().Err(); err != nil {
			return err
		}
		result = &hermesReader{home: home, binary: "sqlite3", installed: binding, metadata: metadata}
		return c.Context().Err()
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (r *hermesReader) installedValid() bool {
	return r != nil && r.installed.valid(r.binary) && r.metadata.complete()
}

func (r *hermesReader) root(ctx context.Context, fn func(*conversationlife.Call) error) error {
	if r == nil || r.installed == nil {
		return conversationlife.ErrRefused
	}
	return r.installed.owner.Do(ctx, func(c *conversationlife.Call) error {
		if !r.installedValid() {
			return conversationlife.ErrRefused
		}
		if err := c.Context().Err(); err != nil {
			return err
		}
		return errors.Join(fn(c), c.Context().Err())
	})
}

func (r *hermesReader) in(parent *conversationlife.Call, fn func(*conversationlife.Call) error) error {
	if r == nil || r.installed == nil {
		return conversationlife.ErrRefused
	}
	return r.installed.owner.DoIn(parent, func(c *conversationlife.Call) error {
		if !r.installedValid() {
			return conversationlife.ErrRefused
		}
		if err := c.Context().Err(); err != nil {
			return err
		}
		return errors.Join(fn(c), c.Context().Err())
	})
}

func hermesFailureCode(code string, err error) string {
	if code == "" || terminalProviderError(err) {
		return "query_failed"
	}
	return code
}

// The observer sees the exact delegated work context, never this wrapper or
// an ancestor. It is not authority; Done/Deadline/Value are unchanged.
type hermesObservationContext struct {
	context.Context
	stage   string
	observe func(context.Context, string)
}

func (c hermesObservationContext) Err() error {
	c.observe(c.Context, c.stage)
	return c.Context.Err()
}

func (r *hermesReader) workContext(c *conversationlife.Call, stage string) context.Context {
	ctx := c.Context()
	if r.observe == nil {
		return ctx
	}
	return hermesObservationContext{Context: ctx, stage: stage, observe: r.observe}
}

func (r *hermesReader) databasesIn(parent *conversationlife.Call) (paths []string, code string, err error) {
	err = r.in(parent, func(c *conversationlife.Call) error {
		ctx := r.workContext(c, "databases")
		if err := ctx.Err(); err != nil {
			return err
		}
		ready, err := r.installed.readyIn(c, r.binary)
		if err != nil {
			return err
		}
		if !ready {
			code = "source_unavailable"
			return nil
		}
		var roots, candidates []string
		if err := sourceEffect(ctx, func() (err error) { roots, err = r.metadata.roots(ctx, r.home); return err }); err != nil {
			return err
		}
		if err := sourceEffect(ctx, func() (err error) { candidates, err = r.metadata.databases(ctx, r.home); return err }); err != nil {
			return err
		}
		for index, candidate := range candidates {
			if err := ctx.Err(); err != nil {
				return err
			}
			if index >= len(roots) {
				break
			}
			path, err := r.metadata.contained(ctx, candidate, roots[index])
			if terminalProviderError(err) {
				return err
			}
			if err == nil && path != "" {
				paths = append(paths, path)
			}
		}
		if len(paths) == 0 {
			code = "source_unavailable"
		}
		return ctx.Err()
	})
	if err != nil {
		return nil, "source_unavailable", err
	}
	return
}

func (r *hermesReader) sameDirectoryIn(parent *conversationlife.Call, cwd, directory string) (same bool, err error) {
	err = r.in(parent, func(c *conversationlife.Call) error {
		ctx := r.workContext(c, "cwd")
		var realCwd, realDirectory string
		cwdErr := sourceEffect(ctx, func() (err error) { realCwd, err = r.metadata.eval(filepath.Clean(cwd)); return err })
		if terminalProviderError(cwdErr) {
			return cwdErr
		}
		directoryErr := sourceEffect(ctx, func() (err error) { realDirectory, err = r.metadata.eval(filepath.Clean(directory)); return err })
		if terminalProviderError(directoryErr) {
			return directoryErr
		}
		same = cwdErr == nil && directoryErr == nil && realCwd == realDirectory
		return ctx.Err()
	})
	if err != nil {
		return false, err
	}
	return
}

func (r *hermesReader) locateIn(parent *conversationlife.Call, cwd, sessionID string) (location Location, code string, err error) {
	err = r.in(parent, func(c *conversationlife.Call) error {
		ctx := r.workContext(c, "locate")
		if err := ctx.Err(); err != nil {
			return err
		}
		if !safeSessionID(sessionID) {
			code = "invalid_session"
			return nil
		}
		paths, discoveryCode, err := r.databasesIn(c)
		if err != nil || discoveryCode != "" {
			code = discoveryCode
			return err
		}
		var firstCode string
		var firstErr error
		for _, database := range paths {
			if err := ctx.Err(); err != nil {
				return err
			}
			rows, _, queryCode, queryErr := r.queryIn(c, database, sessionID, "", 1)
			if err := errors.Join(queryErr, ctx.Err()); terminalProviderError(err) {
				return err
			}
			if queryCode != "" {
				if firstCode == "" {
					firstCode, firstErr = queryCode, queryErr
				}
				continue
			}
			if queryErr != nil {
				return queryErr
			}
			if len(rows) == 0 || rows[0].SessionID != sessionID {
				continue
			}
			if cwd != "" && rows[0].CWD != "" {
				same, err := r.sameDirectoryIn(c, cwd, rows[0].CWD)
				if err != nil {
					return err
				}
				if !same {
					code = "invalid_session"
					return nil
				}
			}
			location = Location{Path: database, Root: filepath.Dir(database), Title: rows[0].Title}
			return ctx.Err()
		}
		code = firstCode
		if code == "" {
			code = "invalid_session"
		}
		return firstErr
	})
	if err != nil {
		return Location{}, hermesFailureCode(code, err), err
	}
	return
}

func (r *hermesReader) queryIn(parent *conversationlife.Call, database, sessionID, before string, limit int) (rows []hermesRow, more bool, code string, err error) {
	err = r.in(parent, func(c *conversationlife.Call) error {
		ctx := r.workContext(c, "message-rows")
		if err := ctx.Err(); err != nil {
			return err
		}
		query, boundedLimit, queryCode := hermesMessageSQL(sessionID, before, limit)
		code = queryCode
		if err := ctx.Err(); err != nil {
			return err
		}
		if code != "" {
			return nil
		}
		raw, queryCode, err := r.installed.queryIn(c, r.binary, database, query, maxHermesOutput, hermesQueryTimeout)
		code = queryCode
		if err != nil || code != "" {
			return err
		}
		rows, more, code, err = decodeHermesRows(ctx, raw, boundedLimit)
		return err
	})
	if err != nil {
		return nil, false, hermesFailureCode(code, err), err
	}
	return
}

func (r *hermesReader) queryToolRowsIn(parent *conversationlife.Call, database, sessionID string, callIDs []string) (rows []hermesToolRow, code string, err error) {
	err = r.in(parent, func(c *conversationlife.Call) error {
		ctx := r.workContext(c, "tool-rows")
		query, err := hermesToolSQL(ctx, sessionID, callIDs)
		if err != nil {
			return err
		}
		raw, queryCode, err := r.installed.queryIn(c, r.binary, database, query, maxHermesOutput, hermesQueryTimeout)
		code = queryCode
		if err != nil || code != "" {
			return err
		}
		rows, code, err = decodeHermesToolRows(ctx, raw)
		return err
	})
	if err != nil {
		return nil, hermesFailureCode(code, err), err
	}
	return
}

// A successful return publishes a private candidate, not mutations of entries.
// All error/cancellation paths discard it. No rollback after publication is
// promised, and no owner/cache lock spans copying, parsing or callbacks.
func (r *hermesReader) attachToolResultsIn(parent *conversationlife.Call, database, sessionID string, entries []Entry) (candidate []Entry, corrupt bool, err error) {
	err = r.in(parent, func(c *conversationlife.Call) error {
		ctx := r.workContext(c, "attachment")
		if err := ctx.Err(); err != nil {
			return err
		}
		if entries != nil {
			candidate = make([]Entry, len(entries))
		}
		for i := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			candidate[i] = entries[i]
			if entries[i].Tools != nil {
				candidate[i].Tools = make([]ToolActivity, len(entries[i].Tools))
			}
			for j := range entries[i].Tools {
				if err := ctx.Err(); err != nil {
					return err
				}
				candidate[i].Tools[j] = entries[i].Tools[j]
			}
		}
		var err error
		corrupt, err = attachHermesTools(ctx, candidate, func(ids []string) ([]hermesToolRow, string, error) {
			if err := ctx.Err(); err != nil {
				return nil, "query_failed", err
			}
			// Only this privately collected IN-set is sorted. Direct query
			// inputs and ordinary map iteration retain their existing order.
			sort.Strings(ids)
			if err := ctx.Err(); err != nil {
				return nil, "query_failed", err
			}
			return r.queryToolRowsIn(c, database, sessionID, ids)
		})
		return errors.Join(err, ctx.Err())
	})
	if err != nil {
		return nil, false, err
	}
	return
}

func (r *hermesReader) readFromIn(parent *conversationlife.Call, database, sessionID, before string, limit int) (entries []Entry, more, corrupt bool, metadata hermesRow, code string, err error) {
	err = r.in(parent, func(c *conversationlife.Call) error {
		ctx := r.workContext(c, "parser")
		if err := ctx.Err(); err != nil {
			return err
		}
		if !safeSessionID(sessionID) {
			code = "invalid_session"
			return nil
		}
		if before != "" {
			cursor, err := strconv.ParseInt(before, 10, 64)
			if err != nil || cursor <= 0 {
				code = "invalid_cursor"
				return nil
			}
		}
		rows, hasMore, queryCode, err := r.queryIn(c, database, sessionID, before, limit)
		code = queryCode
		if err != nil || code != "" {
			return err
		}
		if len(rows) == 0 || rows[0].SessionID != sessionID {
			code = "invalid_session"
			return nil
		}
		if before != "" && rows[0].CursorFound == 0 {
			code = "invalid_cursor"
			return nil
		}
		parsed, damaged, err := parseHermesRowsContext(ctx, rows)
		if err != nil {
			return err
		}
		candidate, toolCorrupt, err := r.attachToolResultsIn(c, database, sessionID, parsed)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, more, corrupt, metadata = candidate, hasMore, damaged || toolCorrupt, rows[0]
		return nil
	})
	if err != nil {
		return nil, false, false, hermesRow{}, hermesFailureCode(code, err), err
	}
	return
}

func decodeHermesToolRows(ctx context.Context, raw []byte) ([]hermesToolRow, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "query_failed", err
	}
	var rows []hermesToolRow
	decodeErr := json.Unmarshal(raw, &rows)
	if err := ctx.Err(); err != nil {
		return nil, "query_failed", err
	}
	if decodeErr != nil {
		return nil, "source_corrupt", decodeErr
	}
	return rows, "", nil
}

func parseHermesRows(rows []hermesRow) ([]Entry, bool) {
	entries, corrupt, _ := parseHermesRowsContext(context.Background(), rows)
	return entries, corrupt
}

func parseHermesRowsContext(ctx context.Context, rows []hermesRow) (result []Entry, damaged bool, err error) {
	defer func() {
		if cancelled := ctx.Err(); cancelled != nil {
			result, damaged, err = nil, false, cancelled
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	entries := make([]Entry, 0, len(rows))
	corrupt := false
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		if row.MessageID <= 0 || (row.Role != "user" && row.Role != "assistant") {
			continue
		}
		tools, toolsCorrupt, err := parseHermesToolCallsContext(ctx, row.ToolCalls)
		if err != nil {
			return nil, false, err
		}
		textValue, textCorrupt, err := hermesTextHexValueContext(ctx, row.ContentHex)
		if err != nil {
			return nil, false, err
		}
		corrupt = corrupt || toolsCorrupt || textCorrupt
		text := sanitizeText(textValue)
		if text == "" && len(tools) == 0 {
			continue
		}
		text, truncated := clampText(text, maxEntryBytes)
		entry := Entry{
			ID:        strconv.FormatInt(row.MessageID, 10),
			Timestamp: hermesTimestamp(row.Timestamp),
			Role:      row.Role,
			Text:      text,
			Tools:     tools,
			Truncated: truncated,
		}
		entries = append(entries, entry)
	}
	return entries, corrupt, nil
}

func parseHermesToolCalls(raw string) ([]ToolActivity, bool) {
	tools, corrupt, _ := parseHermesToolCallsContext(context.Background(), raw)
	return tools, corrupt
}

func parseHermesToolCallsContext(ctx context.Context, raw string) (result []ToolActivity, damaged bool, err error) {
	defer func() {
		if cancelled := ctx.Err(); cancelled != nil {
			result, damaged, err = nil, false, cancelled
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return nil, false, nil
	}
	var decoded any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return nil, true, nil
	}
	var calls []any
	switch value := decoded.(type) {
	case []any:
		calls = value
	case map[string]any:
		if nested, ok := value["tool_calls"].([]any); ok {
			calls = nested
		} else {
			calls = []any{value}
		}
	default:
		return nil, true, nil
	}
	activities := make([]ToolActivity, 0, len(calls))
	corrupt := false
	for _, rawCall := range calls {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		call, ok := rawCall.(map[string]any)
		if !ok {
			corrupt = true
			continue
		}
		function, _ := call["function"].(map[string]any)
		id := firstString(call, "id", "call_id", "tool_call_id")
		name := firstString(call, "name", "tool_name")
		input := firstValue(call, "arguments", "input")
		if function != nil {
			if id == "" {
				id = firstString(function, "id", "call_id", "tool_call_id")
			}
			if name == "" {
				name = firstString(function, "name", "tool_name")
			}
			if input == nil {
				input = firstValue(function, "arguments", "input")
			}
		}
		inputText, err := providerTextValueContext(ctx, input)
		if err != nil {
			return nil, false, err
		}
		activity := newToolActivity(id, name, inputText)
		activity.State = "running"
		activities = append(activities, activity)
	}
	return activities, corrupt, nil
}

func hermesText(raw string) string {
	value := any(raw)
	if strings.HasPrefix(raw, "\x00json:") {
		var decoded any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(raw, "\x00json:")), &decoded); err == nil {
			value = decoded
		}
	}
	return textValue(value)
}

func hermesTextHex(encoded string) string {
	text, _ := hermesTextHexValue(encoded)
	return text
}

func hermesTextHexValue(encoded string) (string, bool) {
	text, corrupt, _ := hermesTextHexValueContext(context.Background(), encoded)
	return text, corrupt
}

func hermesTextHexValueContext(ctx context.Context, encoded string) (text string, corrupt bool, err error) {
	defer func() {
		if cancelled := ctx.Err(); cancelled != nil {
			text, corrupt, err = "", false, cancelled
		}
	}()
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	if encoded == "" {
		return "", false, nil
	}
	decoded, err := hex.DecodeString(encoded)
	if err != nil {
		return "", true, nil
	}
	raw := string(decoded)
	value := any(raw)
	if strings.HasPrefix(raw, "\x00json:") {
		var decoded any
		if json.Unmarshal([]byte(strings.TrimPrefix(raw, "\x00json:")), &decoded) == nil {
			value = decoded
		}
	}
	text, err = providerTextValueContext(ctx, value)
	return text, false, err
}

// Context-aware counterpart of textValue/textBlockList for provider-owned
// arbitrary JSON inputs. Keep the frozen Reader's stateless API unchanged.
func providerTextValueContext(ctx context.Context, value any) (result string, err error) {
	defer func() {
		if cancelled := ctx.Err(); cancelled != nil {
			result, err = "", cancelled
		}
	}()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if text, ok := value.(string); ok {
		return text, nil
	}
	if blocks, ok := value.([]any); ok {
		texts := make([]string, 0, len(blocks))
		for _, raw := range blocks {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			block, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			kind := stringValue(block["type"])
			if kind != "text" && kind != "input_text" && kind != "output_text" {
				continue
			}
			if text := stringValue(block["text"]); text != "" {
				texts = append(texts, text)
			}
		}
		if text := strings.Join(texts, "\n"); text != "" {
			return text, nil
		}
	}
	data, marshalErr := json.Marshal(value)
	if marshalErr != nil || string(data) == "null" {
		return "", nil
	}
	return string(data), nil
}

func hermesTimestamp(value float64) string {
	if value <= 0 {
		return ""
	}
	return time.Unix(0, int64(value*float64(time.Second))).UTC().Format(time.RFC3339Nano)
}

func isHermesAgent(agent string) bool {
	switch normalizedAgent(agent) {
	case "hermes", "hermesagent":
		return true
	default:
		return false
	}
}

func (r *Reader) readHermesFor(agent, cwd, sessionID, before string, limit int) (Page, error) {
	sessionID = strings.TrimSpace(sessionID)
	if !safeSessionID(sessionID) {
		return unavailableCode("invalid_session", "This agent has not reported a conversation session yet."), nil
	}
	if before != "" {
		cursor, err := strconv.ParseInt(before, 10, 64)
		if err != nil || cursor <= 0 {
			return unavailableCode("invalid_cursor", "This conversation page cursor is invalid."), nil
		}
	}
	if limit < 1 {
		limit = defaultPageSize
	}
	if limit > maxPageSize {
		limit = maxPageSize
	}
	location := r.Locate(agent, cwd, sessionID)
	if location.Path == "" {
		if _, code := r.hermes.databases(); code != "" {
			return unavailableCode(code, "Hermes conversation history is unavailable."), nil
		}
		return unavailableCode("invalid_session", "No conversation log is available for this session."), nil
	}
	entries, hasMore, corrupt, metadata, code := r.hermes.readFrom(location.Path, sessionID, before, limit)
	if code != "" {
		return unavailableCode(code, "Hermes conversation history is unavailable."), nil
	}
	if cwd != "" && metadata.CWD != "" && !sameHermesDirectory(cwd, metadata.CWD) {
		return unavailableCode("invalid_session", "This conversation belongs to a different workspace."), nil
	}
	normalizeEntriesForResponse(entries)
	return Page{
		Available:     true,
		Entries:       append([]Entry(nil), entries...),
		HasMore:       hasMore,
		Total:         metadata.Total,
		SourceCorrupt: corrupt,
	}, nil
}
