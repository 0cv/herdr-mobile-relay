package conversation

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
	"github.com/0cv/herdr-mobile-relay/internal/conversationlife"
	"github.com/0cv/herdr-mobile-relay/internal/sqliteexec"
)

const (
	openCodeQueryTimeout = 10 * time.Second
	maxOpenCodeOutput    = 8 * 1024 * 1024
	maxOpenCodeCache     = 128
)

type openCodeReader struct {
	home      string
	binary    string
	executor  sqliteexec.Executor
	mu        sync.Mutex
	cache     map[string]openCodeCacheEntry
	layouts   map[string]openCodeLayoutEntry
	installed *installedProviderSQL
	metadata  installedProviderMetadata
	// Optional returning observations, configured before use; never under mu.
	observe       func(string)
	parserObserve func(context.Context)
}

type openCodeFileStamp struct {
	databaseSize int64
	databaseTime int64
	walSize      int64
	walTime      int64
}

type openCodeCacheEntry struct {
	stamp   openCodeFileStamp
	rows    []openCodeRow
	hasMore bool
}

type openCodeRow struct {
	SessionID   string `json:"session_id"`
	Directory   string `json:"directory"`
	Title       string `json:"title"`
	Updated     int64  `json:"time_updated"`
	Agent       string `json:"agent"`
	MessageID   string `json:"message_id"`
	Created     int64  `json:"time_created"`
	MessageData string `json:"message_data"`
	PartID      string `json:"part_id"`
	PartData    string `json:"part_data"`
	Total       int    `json:"message_total"`
	CursorFound int    `json:"cursor_found"`
	MessageType string `json:"message_type"`
	HasMessages int    `json:"has_messages"`
	Database    string `json:"-"`
	V2          bool   `json:"-"`
}

func newOpenCodeReader(home string) *openCodeReader {
	return &openCodeReader{home: home, binary: "sqlite3", executor: sqliteexec.MustFromEnv(), cache: make(map[string]openCodeCacheEntry), layouts: make(map[string]openCodeLayoutEntry)}
}

func (r *openCodeReader) queryExec() sqliteexec.Executor {
	if r.installed != nil {
		return nil
	}
	return resolveQueryExecutor(r.executor, r.binary)
}

func (r *openCodeReader) databases() ([]string, string) {
	if r.installed != nil {
		var paths []string
		var code string
		err := r.root(context.Background(), func(c *conversationlife.Call) error { var err error; paths, code, err = r.databasesIn(c); return err })
		if err != nil {
			return nil, "source_unavailable"
		}
		return paths, code
	}
	if exec := r.queryExec(); exec == nil || !exec.Ready() {
		return nil, "source_unavailable"
	}
	roots := agentroots.OpenCodeData(r.home)
	databases := make([]string, 0, len(roots))
	for index, candidate := range agentroots.OpenCodeDBs(r.home) {
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

func (r *openCodeReader) read(sessionID, before string, limit int) ([]Entry, bool, bool, openCodeRow, string) {
	return r.readContext(context.Background(), sessionID, before, limit)
}

func (r *openCodeReader) readContext(ctx context.Context, sessionID, before string, limit int) ([]Entry, bool, bool, openCodeRow, string) {
	if r.installed != nil {
		var entries []Entry
		var more, corrupt bool
		var metadata openCodeRow
		var code string
		err := r.root(ctx, func(c *conversationlife.Call) error {
			var err error
			entries, more, corrupt, metadata, code, err = r.readIn(c, sessionID, before, limit)
			return err
		})
		if err != nil {
			if code == "" || terminalProviderError(err) {
				code = "query_failed"
			}
			return nil, false, false, openCodeRow{}, code
		}
		return entries, more, corrupt, metadata, code
	}
	if !validOpenCodeSessionID(sessionID) {
		return nil, false, false, openCodeRow{}, "invalid_session"
	}
	if before != "" && (len(before) > 256 || strings.ContainsAny(before, "\x00\r\n")) {
		return nil, false, false, openCodeRow{}, "invalid_cursor"
	}
	databases, code := r.databases()
	if code != "" {
		return nil, false, false, openCodeRow{}, code
	}
	firstFailure := ""
	for _, database := range databases {
		rows, hasMore, queryCode := r.queryContext(ctx, database, sessionID, before, limit)
		if queryCode != "" {
			if firstFailure == "" {
				firstFailure = queryCode
			}
			continue
		}
		if len(rows) == 0 || rows[0].SessionID != sessionID {
			continue
		}
		if before != "" && rows[0].CursorFound == 0 {
			return nil, false, false, openCodeRow{}, "invalid_cursor"
		}
		metadata := rows[0]
		metadata.Database = database
		entries, corrupt := parseOpenCodeRows(rows)
		return entries, hasMore, corrupt, metadata, ""
	}
	if firstFailure != "" {
		return nil, false, false, openCodeRow{}, firstFailure
	}
	return nil, false, false, openCodeRow{}, "invalid_session"
}

func openCodeStamp(database string) (openCodeFileStamp, bool) {
	info, err := os.Stat(database)
	if err != nil {
		return openCodeFileStamp{}, false
	}
	stamp := openCodeFileStamp{databaseSize: info.Size(), databaseTime: info.ModTime().UnixNano()}
	if info, err = os.Stat(database + "-wal"); err == nil {
		stamp.walSize = info.Size()
		stamp.walTime = info.ModTime().UnixNano()
	}
	return stamp, true
}

func (r *openCodeReader) cachedQuery(key string, stamp openCodeFileStamp) ([]openCodeRow, bool, bool) {
	if r.installed != nil {
		var rows []openCodeRow
		var more, hit bool
		err := r.root(context.Background(), func(c *conversationlife.Call) error {
			var err error
			rows, more, hit, err = r.cachedQueryIn(c, key, stamp)
			return err
		})
		if err != nil {
			return nil, false, false
		}
		return rows, more, hit
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.cache[key]
	if !ok || entry.stamp != stamp {
		return nil, false, false
	}
	return append([]openCodeRow(nil), entry.rows...), entry.hasMore, true
}

func (r *openCodeReader) storeQuery(key string, stamp openCodeFileStamp, rows []openCodeRow, hasMore bool) {
	if r.installed != nil {
		_ = r.root(context.Background(), func(c *conversationlife.Call) error { return r.storeQueryIn(c, key, stamp, rows, hasMore) })
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.cache) >= maxOpenCodeCache {
		clear(r.cache)
	}
	r.cache[key] = openCodeCacheEntry{
		stamp: stamp, rows: append([]openCodeRow(nil), rows...), hasMore: hasMore,
	}
}

func (r *openCodeReader) query(database, sessionID, before string, limit int) ([]openCodeRow, bool, string) {
	return r.queryContext(context.Background(), database, sessionID, before, limit)
}

func (r *openCodeReader) queryContext(ctx context.Context, database, sessionID, before string, limit int) ([]openCodeRow, bool, string) {
	if r.installed != nil {
		return r.rowsRoot(ctx, func(c *conversationlife.Call) ([]openCodeRow, bool, string, error) {
			return r.queryIn(c, database, sessionID, before, limit)
		})
	}
	stamp, stampOK := openCodeStamp(database)
	cacheKey := fmt.Sprintf("%s\x00%s\x00%s\x00%d", database, sessionID, before, limit)
	if stampOK {
		if rows, hasMore, ok := r.cachedQuery(cacheKey, stamp); ok {
			return rows, hasMore, ""
		}
	}
	layout, code := r.layoutContext(ctx, database, stamp, stampOK)
	if code != "" {
		return nil, false, code
	}
	var rows []openCodeRow
	var hasMore bool
	switch {
	case layout.v2 && layout.v1:
		// Upgraded store: OpenCode migrates sessions lazily, so a session
		// with 2.x turns is read from the 2.x tables and any other session
		// from the 1.x tables.
		rows, hasMore, code = r.queryV2(ctx, database, sessionID, before, limit)
		if code != "" {
			return nil, false, code
		}
		if len(rows) > 0 && rows[0].HasMessages != 0 {
			break
		}
		v1Rows, v1More, v1Code := r.queryV1(ctx, database, sessionID, before, limit)
		if v1Code != "" {
			return nil, false, v1Code
		}
		if len(v1Rows) > 0 || len(rows) == 0 {
			rows, hasMore = v1Rows, v1More
		}
	case layout.v2:
		rows, hasMore, code = r.queryV2(ctx, database, sessionID, before, limit)
	default:
		rows, hasMore, code = r.queryV1(ctx, database, sessionID, before, limit)
	}
	if code != "" {
		return nil, false, code
	}
	if current, ok := openCodeStamp(database); stampOK && ok && current == stamp {
		r.storeQuery(cacheKey, stamp, rows, hasMore)
	}
	return rows, hasMore, ""
}

// openCodeLayout records which OpenCode table sets a database holds. 1.x uses
// session/message/part; 2.x uses session_v2/session_message. A store upgraded
// from 1.x keeps both.
type openCodeLayout struct {
	v1 bool
	v2 bool
}

type openCodeLayoutEntry struct {
	stamp  openCodeFileStamp
	layout openCodeLayout
}

func (r *openCodeReader) layoutContext(ctx context.Context, database string, stamp openCodeFileStamp, stampOK bool) (openCodeLayout, string) {
	if r.installed != nil {
		var layout openCodeLayout
		var code string
		err := r.root(ctx, func(c *conversationlife.Call) error {
			var err error
			layout, code, err = r.layoutIn(c, database, stamp, stampOK)
			return err
		})
		if err != nil {
			return openCodeLayout{}, "query_failed"
		}
		return layout, code
	}
	if stampOK {
		r.mu.Lock()
		entry, ok := r.layouts[database]
		r.mu.Unlock()
		if ok && entry.stamp == stamp {
			return entry.layout, ""
		}
	}
	queryCtx, cancel := context.WithTimeout(ctx, openCodeQueryTimeout)
	defer cancel()
	raw, code := runSQLiteJSON(queryCtx, r.queryExec(), database,
		`SELECT name FROM sqlite_master WHERE type='table' AND name IN ('session','message','part','session_v2','session_message');`,
		maxOpenCodeOutput)
	if code != "" {
		return openCodeLayout{}, code
	}
	layout, code, _ := decodeOpenCodeLayout(context.Background(), raw, nil)
	if code != "" {
		return openCodeLayout{}, code
	}
	if current, ok := openCodeStamp(database); stampOK && ok && current == stamp {
		r.mu.Lock()
		if len(r.layouts) >= maxOpenCodeCache {
			clear(r.layouts)
		}
		r.layouts[database] = openCodeLayoutEntry{stamp: stamp, layout: layout}
		r.mu.Unlock()
	}
	return layout, ""
}

func (r *openCodeReader) queryV1(ctx context.Context, database, sessionID, before string, limit int) ([]openCodeRow, bool, string) {
	return r.runRows(ctx, database, openCodeV1SQL(sessionID, before, limit), limit, false)
}

func openCodeV1SQL(sessionID, before string, limit int) string {
	sessionHex := hex.EncodeToString([]byte(sessionID))
	cursorCTE := `cursor AS (SELECT NULL AS time_created,NULL AS id WHERE 0)`
	cursorFilter := ""
	cursorFound := "1"
	if before != "" {
		cursorHex := hex.EncodeToString([]byte(before))
		cursorCTE = fmt.Sprintf(
			`cursor AS (SELECT time_created,id FROM message WHERE session_id=CAST(X'%s' AS TEXT) AND id=CAST(X'%s' AS TEXT))`,
			sessionHex,
			cursorHex,
		)
		cursorFilter = ` AND EXISTS(SELECT 1 FROM cursor) AND (m.time_created < (SELECT time_created FROM cursor) OR (m.time_created = (SELECT time_created FROM cursor) AND m.id < (SELECT id FROM cursor)))`
		cursorFound = "EXISTS(SELECT 1 FROM cursor)"
	}
	query := fmt.Sprintf(
		`WITH %s,selected AS (`+
			`SELECT m.id,m.time_created,m.data FROM message AS m WHERE m.session_id=CAST(X'%s' AS TEXT)%s `+
			`ORDER BY m.time_created DESC,m.id DESC LIMIT %d`+
			`) SELECT s.id AS session_id,s.directory,s.title,s.time_updated,COALESCE(s.agent,'') AS agent,`+
			`COALESCE(sm.id,'') AS message_id,COALESCE(sm.time_created,0) AS time_created,COALESCE(sm.data,'null') AS message_data,`+
			`COALESCE(p.id,'') AS part_id,COALESCE(p.data,'null') AS part_data,`+
			`(SELECT COUNT(*) FROM message WHERE session_id=s.id) AS message_total,%s AS cursor_found `+
			`FROM session AS s LEFT JOIN selected AS sm ON 1=1 LEFT JOIN part AS p ON p.message_id=sm.id `+
			`WHERE s.id=CAST(X'%s' AS TEXT) ORDER BY sm.time_created DESC,sm.id DESC,p.id DESC;`,
		cursorCTE,
		sessionHex,
		cursorFilter,
		limit+1,
		cursorFound,
		sessionHex,
	)
	return query
}

// queryV2 reads the OpenCode 2.x tables. Only user and assistant turns are
// selected, in SQL before LIMIT, so hidden turn types neither use page slots
// nor count toward the total. Turn order is session_message.seq.
func (r *openCodeReader) queryV2(ctx context.Context, database, sessionID, before string, limit int) ([]openCodeRow, bool, string) {
	return r.runRows(ctx, database, openCodeV2SQL(sessionID, before, limit), limit, true)
}

func openCodeV2SQL(sessionID, before string, limit int) string {
	const visible = `type IN ('user','assistant')`
	sessionHex := hex.EncodeToString([]byte(sessionID))
	cursorCTE := `cursor AS (SELECT NULL AS seq WHERE 0)`
	cursorFilter := ""
	cursorFound := "1"
	if before != "" {
		cursorHex := hex.EncodeToString([]byte(before))
		cursorCTE = fmt.Sprintf(
			`cursor AS (SELECT seq FROM session_message WHERE session_id=CAST(X'%s' AS TEXT) AND id=CAST(X'%s' AS TEXT) AND %s)`,
			sessionHex,
			cursorHex,
			visible,
		)
		cursorFilter = ` AND EXISTS(SELECT 1 FROM cursor) AND m.seq < (SELECT seq FROM cursor)`
		cursorFound = "EXISTS(SELECT 1 FROM cursor)"
	}
	query := fmt.Sprintf(
		`WITH %s,selected AS (`+
			`SELECT m.id,m.type,m.seq,m.time_created,m.data FROM session_message AS m `+
			`WHERE m.session_id=CAST(X'%s' AS TEXT) AND m.%s%s ORDER BY m.seq DESC LIMIT %d`+
			`) SELECT s.id AS session_id,COALESCE(s.directory,'') AS directory,COALESCE(s.title,'') AS title,`+
			`COALESCE(s.time_updated,0) AS time_updated,COALESCE(s.agent,'') AS agent,`+
			`COALESCE(sm.id,'') AS message_id,COALESCE(sm.type,'') AS message_type,COALESCE(sm.time_created,0) AS time_created,`+
			`COALESCE(sm.data,'null') AS message_data,`+
			`(SELECT COUNT(*) FROM session_message WHERE session_id=s.id AND %s) AS message_total,`+
			`EXISTS(SELECT 1 FROM session_message WHERE session_id=s.id) AS has_messages,%s AS cursor_found `+
			`FROM session_v2 AS s LEFT JOIN selected AS sm ON 1=1 `+
			`WHERE s.id=CAST(X'%s' AS TEXT) ORDER BY sm.seq DESC;`,
		cursorCTE,
		sessionHex,
		visible,
		cursorFilter,
		limit+1,
		visible,
		cursorFound,
		sessionHex,
	)
	return query
}

func (r *openCodeReader) runRows(ctx context.Context, database, query string, limit int, v2 bool) ([]openCodeRow, bool, string) {
	if r.installed != nil {
		return r.rowsRoot(ctx, func(c *conversationlife.Call) ([]openCodeRow, bool, string, error) {
			return r.runRowsIn(c, database, query, limit, v2)
		})
	}
	queryCtx, cancel := context.WithTimeout(ctx, openCodeQueryTimeout)
	defer cancel()
	raw, code := runSQLiteJSON(queryCtx, r.queryExec(), database, query, maxOpenCodeOutput)
	if code != "" {
		return nil, false, code
	}
	rows, more, code, _ := decodeOpenCodeRows(context.Background(), raw, limit, v2, nil)
	return rows, more, code
}

// Shared row algorithm; ordinary callers retain their historical contextless
// decoding, while installed callers retain the admitted operation throughout.
func decodeOpenCodeRows(ctx context.Context, raw []byte, limit int, v2 bool, observe func(string)) ([]openCodeRow, bool, string, error) {
	check := func(stage string) error {
		if observe != nil {
			observe(stage)
		}
		return ctx.Err()
	}
	if err := check("decode"); err != nil {
		return nil, false, "query_failed", err
	}
	var rows []openCodeRow
	decodeErr := json.Unmarshal(raw, &rows)
	if err := ctx.Err(); err != nil {
		return nil, false, "query_failed", err
	}
	if decodeErr != nil {
		return nil, false, "source_corrupt", nil
	}
	for left, right := 0, len(rows)-1; left < right; left, right = left+1, right-1 {
		if err := check("reverse"); err != nil {
			return nil, false, "query_failed", err
		}
		rows[left], rows[right] = rows[right], rows[left]
	}
	for index := range rows {
		if err := check("version"); err != nil {
			return nil, false, "query_failed", err
		}
		rows[index].V2 = v2
	}
	messageIDs := make([]string, 0, limit+1)
	seen := make(map[string]struct{}, limit+1)
	for _, row := range rows {
		if err := check("distinct"); err != nil {
			return nil, false, "query_failed", err
		}
		if row.MessageID == "" {
			continue
		}
		if _, exists := seen[row.MessageID]; exists {
			continue
		}
		seen[row.MessageID] = struct{}{}
		messageIDs = append(messageIDs, row.MessageID)
	}
	hasMore := len(messageIDs) > limit
	if hasMore {
		extraID := messageIDs[0]
		kept := rows[:0]
		for _, row := range rows {
			if err := check("filter"); err != nil {
				return nil, false, "query_failed", err
			}
			if row.MessageID != extraID {
				kept = append(kept, row)
			}
		}
		rows = kept
	}
	if err := ctx.Err(); err != nil {
		return nil, false, "query_failed", err
	}
	return rows, hasMore, "", nil
}

func decodeOpenCodeLayout(ctx context.Context, raw []byte, observe func(string)) (openCodeLayout, string, error) {
	if err := ctx.Err(); err != nil {
		return openCodeLayout{}, "query_failed", err
	}
	var tables []struct {
		Name string `json:"name"`
	}
	decodeErr := json.Unmarshal(raw, &tables)
	if err := ctx.Err(); err != nil {
		return openCodeLayout{}, "query_failed", err
	}
	if decodeErr != nil {
		return openCodeLayout{}, "source_corrupt", nil
	}
	present := make(map[string]bool, len(tables))
	for _, table := range tables {
		if observe != nil {
			observe("layout-row")
		}
		if err := ctx.Err(); err != nil {
			return openCodeLayout{}, "query_failed", err
		}
		present[table.Name] = true
	}
	layout := openCodeLayout{v1: present["session"] && present["message"] && present["part"], v2: present["session_v2"] && present["session_message"]}
	if !layout.v1 && !layout.v2 {
		return openCodeLayout{}, "query_failed", nil
	}
	if err := ctx.Err(); err != nil {
		return openCodeLayout{}, "query_failed", err
	}
	return layout, "", nil
}

// Production association is concrete and precedes admission/effects. This is
// intended for the future Reader constructor's live construction Call.
func newInstalledOpenCodeReaderIn(owner *conversationlife.Owner, parent *conversationlife.Call, home string, executor *sqliteexec.InstalledExecutor) (*openCodeReader, error) {
	binding, err := bindInstalledProviderSQL(owner, executor, "sqlite3")
	if err != nil {
		return nil, err
	}
	return constructInstalledOpenCodeIn(parent, home, binding, productionOpenCodeMetadata())
}

func constructInstalledOpenCodeIn(parent *conversationlife.Call, home string, binding *installedProviderSQL, metadata installedProviderMetadata) (*openCodeReader, error) {
	if !binding.valid("sqlite3") || !metadata.complete() {
		return nil, conversationlife.ErrRefused
	}
	var result *openCodeReader
	err := binding.owner.DoIn(parent, func(c *conversationlife.Call) error {
		if err := c.Context().Err(); err != nil {
			return err
		}
		result = &openCodeReader{home: home, binary: "sqlite3", installed: binding, metadata: metadata,
			cache: make(map[string]openCodeCacheEntry), layouts: make(map[string]openCodeLayoutEntry)}
		return c.Context().Err()
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (r *openCodeReader) installedValid() bool {
	return r != nil && r.installed.valid(r.binary) && r.metadata.complete() && r.cache != nil && r.layouts != nil
}

func (r *openCodeReader) root(ctx context.Context, fn func(*conversationlife.Call) error) error {
	if !r.installedValid() {
		return conversationlife.ErrRefused
	}
	return r.installed.owner.Do(ctx, func(c *conversationlife.Call) error {
		if err := c.Context().Err(); err != nil {
			return err
		}
		return errors.Join(fn(c), c.Context().Err())
	})
}

func (r *openCodeReader) in(parent *conversationlife.Call, fn func(*conversationlife.Call) error) error {
	if !r.installedValid() {
		return conversationlife.ErrRefused
	}
	return r.installed.owner.DoIn(parent, func(c *conversationlife.Call) error {
		if err := c.Context().Err(); err != nil {
			return err
		}
		return errors.Join(fn(c), c.Context().Err())
	})
}

func (r *openCodeReader) rowsRoot(ctx context.Context, fn func(*conversationlife.Call) ([]openCodeRow, bool, string, error)) ([]openCodeRow, bool, string) {
	var rows []openCodeRow
	var more bool
	var code string
	err := r.root(ctx, func(c *conversationlife.Call) error { var err error; rows, more, code, err = fn(c); return err })
	if err != nil {
		if code == "" || terminalProviderError(err) {
			code = "query_failed"
		}
		return nil, false, code
	}
	return rows, more, code
}

func (r *openCodeReader) checkpoint(ctx context.Context, stage string) error {
	if r.observe != nil {
		r.observe(stage)
	}
	return ctx.Err()
}

func (r *openCodeReader) databasesIn(parent *conversationlife.Call) (paths []string, code string, err error) {
	err = r.in(parent, func(c *conversationlife.Call) error {
		ctx := c.Context()
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
		return nil
	})
	if err != nil {
		return nil, "source_unavailable", err
	}
	return
}

func (r *openCodeReader) readIn(parent *conversationlife.Call, sessionID, before string, limit int) (entries []Entry, more, corrupt bool, metadata openCodeRow, code string, err error) {
	err = r.in(parent, func(c *conversationlife.Call) error {
		if !validOpenCodeSessionID(sessionID) {
			code = "invalid_session"
			return nil
		}
		if before != "" && (len(before) > 256 || strings.ContainsAny(before, "\x00\r\n")) {
			code = "invalid_cursor"
			return nil
		}
		paths, discoveryCode, err := r.databasesIn(c)
		if err != nil {
			return err
		}
		if discoveryCode != "" {
			code = discoveryCode
			return nil
		}
		firstFailure := ""
		var firstError error
		for _, database := range paths {
			if err := c.Context().Err(); err != nil {
				return err
			}
			rows, hasMore, queryCode, queryErr := r.queryIn(c, database, sessionID, before, limit)
			if terminalProviderError(queryErr) {
				return queryErr
			}
			if err := c.Context().Err(); err != nil {
				return errors.Join(queryErr, err)
			}
			if queryCode != "" {
				if firstFailure == "" {
					firstFailure, firstError = queryCode, queryErr
				}
				continue
			}
			if queryErr != nil {
				return queryErr
			}
			if len(rows) == 0 || rows[0].SessionID != sessionID {
				continue
			}
			if before != "" && rows[0].CursorFound == 0 {
				code = "invalid_cursor"
				return nil
			}
			parserContext := c.Context()
			if r.observe != nil || r.parserObserve != nil {
				parserContext = openCodeParserObservationContext{Context: parserContext, observe: r.observe, contextObserved: r.parserObserve}
			}
			parsed, damaged, err := parseOpenCodeRowsContext(parserContext, rows)
			if err != nil {
				return err
			}
			entries, more, corrupt, metadata = parsed, hasMore, damaged, rows[0]
			metadata.Database = database
			return nil
		}
		code = firstFailure
		if code == "" {
			code = "invalid_session"
		}
		return firstError
	})
	if err != nil {
		if code == "" || terminalProviderError(err) {
			code = "query_failed"
		}
		return nil, false, false, openCodeRow{}, code, err
	}
	return
}

// Observation only: admission remains with the original Call. Embedded context
// delegates Deadline, Done and Value unchanged; production nil uses no wrapper.
type openCodeParserObservationContext struct {
	context.Context
	observe         func(string)
	contextObserved func(context.Context)
}

func (c openCodeParserObservationContext) Err() error {
	if c.contextObserved != nil {
		c.contextObserved(c.Context)
	}
	if c.observe != nil {
		c.observe("parser-check")
	}
	return c.Context.Err()
}

func copyOpenCodeRows(ctx context.Context, rows []openCodeRow, observe func(string)) ([]openCodeRow, error) {
	var copied []openCodeRow
	if rows != nil {
		copied = make([]openCodeRow, len(rows))
	}
	for i := range rows {
		if observe != nil {
			observe("cache-copy")
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		copied[i] = rows[i]
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return copied, nil
}

func (r *openCodeReader) cachedQueryIn(parent *conversationlife.Call, key string, stamp openCodeFileStamp) (rows []openCodeRow, more, hit bool, err error) {
	err = r.in(parent, func(c *conversationlife.Call) error {
		r.mu.Lock()
		entry, ok := r.cache[key]
		r.mu.Unlock()
		if err := r.checkpoint(c.Context(), "cache-hit"); err != nil {
			return err
		}
		if !ok || entry.stamp != stamp {
			return nil
		}
		var err error
		rows, err = copyOpenCodeRows(c.Context(), entry.rows, r.observe)
		if err != nil {
			return err
		}
		more, hit = entry.hasMore, true
		return nil
	})
	if err != nil {
		return nil, false, false, err
	}
	return
}

func (r *openCodeReader) storeQueryIn(parent *conversationlife.Call, key string, stamp openCodeFileStamp, rows []openCodeRow, more bool) error {
	return r.in(parent, func(c *conversationlife.Call) error {
		ctx := c.Context()
		copied, err := copyOpenCodeRows(ctx, rows, r.observe)
		if err != nil {
			return err
		}
		// Keys retain the existing database-first NUL-separated format.
		database, _, _ := strings.Cut(key, "\x00")
		current, ok, err := r.metadata.stamp(ctx, database)
		if err != nil {
			return err
		}
		if !ok || current != stamp {
			return nil
		}
		if err := r.checkpoint(ctx, "cache-publish"); err != nil {
			return err
		}
		done := ctx.Done()
		r.mu.Lock()
		select {
		case <-done:
		default:
			if len(r.cache) >= maxOpenCodeCache {
				clear(r.cache)
			}
			r.cache[key] = openCodeCacheEntry{stamp: stamp, rows: copied, hasMore: more}
		}
		r.mu.Unlock()
		return ctx.Err()
	})
}

func (r *openCodeReader) queryIn(parent *conversationlife.Call, database, sessionID, before string, limit int) (rows []openCodeRow, more bool, code string, err error) {
	err = r.in(parent, func(c *conversationlife.Call) error {
		if limit < 0 {
			return conversationlife.ErrRefused
		}
		ctx := c.Context()
		stamp, stampOK, err := r.metadata.stamp(ctx, database)
		if err != nil {
			return err
		}
		key := fmt.Sprintf("%s\x00%s\x00%s\x00%d", database, sessionID, before, limit)
		if stampOK {
			var hit bool
			rows, more, hit, err = r.cachedQueryIn(c, key, stamp)
			if err != nil {
				return err
			}
			if hit {
				return nil
			}
		}
		layout, layoutCode, err := r.layoutIn(c, database, stamp, stampOK)
		if err != nil || layoutCode != "" {
			code = layoutCode
			return err
		}
		if layout.v2 {
			rows, more, code, err = r.queryV2In(c, database, sessionID, before, limit)
			if err != nil || code != "" {
				return err
			}
			if layout.v1 && (len(rows) == 0 || rows[0].HasMessages == 0) {
				if err := ctx.Err(); err != nil {
					return err
				}
				v1Rows, v1More, v1Code, v1Err := r.queryV1In(c, database, sessionID, before, limit)
				if v1Err != nil || v1Code != "" {
					code = v1Code
					return v1Err
				}
				if len(v1Rows) > 0 || len(rows) == 0 {
					rows, more = v1Rows, v1More
				}
			}
		} else {
			rows, more, code, err = r.queryV1In(c, database, sessionID, before, limit)
			if err != nil || code != "" {
				return err
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if stampOK {
			return r.storeQueryIn(c, key, stamp, rows, more)
		}
		return nil
	})
	if err != nil {
		if code == "" {
			code = "query_failed"
		}
		return nil, false, code, err
	}
	return
}

func (r *openCodeReader) layoutIn(parent *conversationlife.Call, database string, stamp openCodeFileStamp, stampOK bool) (layout openCodeLayout, code string, err error) {
	err = r.in(parent, func(c *conversationlife.Call) error {
		ctx := c.Context()
		if stampOK {
			r.mu.Lock()
			entry, ok := r.layouts[database]
			r.mu.Unlock()
			if err := r.checkpoint(ctx, "layout-hit"); err != nil {
				return err
			}
			if ok && entry.stamp == stamp {
				layout = entry.layout
				return nil
			}
		}
		raw, queryCode, err := r.installed.queryIn(c, r.binary, database,
			`SELECT name FROM sqlite_master WHERE type='table' AND name IN ('session','message','part','session_v2','session_message');`, maxOpenCodeOutput, openCodeQueryTimeout)
		code = queryCode
		if err != nil || code != "" {
			return err
		}
		layout, code, err = decodeOpenCodeLayout(ctx, raw, r.observe)
		if err != nil || code != "" {
			return err
		}
		current, ok, err := r.metadata.stamp(ctx, database)
		if err != nil {
			return err
		}
		if stampOK && ok && current == stamp {
			if err := r.checkpoint(ctx, "layout-publish"); err != nil {
				return err
			}
			done := ctx.Done()
			r.mu.Lock()
			select {
			case <-done:
			default:
				if len(r.layouts) >= maxOpenCodeCache {
					clear(r.layouts)
				}
				r.layouts[database] = openCodeLayoutEntry{stamp: stamp, layout: layout}
			}
			r.mu.Unlock()
		}
		return ctx.Err()
	})
	if err != nil {
		if code == "" {
			code = "query_failed"
		}
		return openCodeLayout{}, code, err
	}
	return
}

func (r *openCodeReader) queryV1In(parent *conversationlife.Call, database, sessionID, before string, limit int) ([]openCodeRow, bool, string, error) {
	return r.runRowsIn(parent, database, openCodeV1SQL(sessionID, before, limit), limit, false)
}

func (r *openCodeReader) queryV2In(parent *conversationlife.Call, database, sessionID, before string, limit int) ([]openCodeRow, bool, string, error) {
	return r.runRowsIn(parent, database, openCodeV2SQL(sessionID, before, limit), limit, true)
}

func (r *openCodeReader) runRowsIn(parent *conversationlife.Call, database, query string, limit int, v2 bool) (rows []openCodeRow, more bool, code string, err error) {
	err = r.in(parent, func(c *conversationlife.Call) error {
		if limit < 0 {
			return conversationlife.ErrRefused
		}
		raw, queryCode, err := r.installed.queryIn(c, r.binary, database, query, maxOpenCodeOutput, openCodeQueryTimeout)
		code = queryCode
		if err != nil || code != "" {
			return err
		}
		rows, more, code, err = decodeOpenCodeRows(c.Context(), raw, limit, v2, r.observe)
		return err
	})
	if err != nil {
		if code == "" {
			code = "query_failed"
		}
		return nil, false, code, err
	}
	return
}

// readOpenCode reads a session by the ID Herdr reported for the pane. It does
// not bind the session to the pane cwd: OpenCode sessions own their stored
// directory, and `opencode -s` resumes them there from any launch directory,
// so the pane cwd is not evidence of ownership. Codex and Pi likewise resolve
// by session ID alone.
func (r *Reader) readOpenCode(sessionID, before string, limit int) (Page, error) {
	sessionID = strings.TrimSpace(sessionID)
	if limit < 1 {
		limit = defaultPageSize
	}
	if limit > maxPageSize {
		limit = maxPageSize
	}
	entries, hasMore, corrupt, metadata, code := r.openCode.read(sessionID, before, limit)
	if code != "" {
		return unavailableCode(code, "OpenCode conversation history is unavailable."), nil
	}
	normalizeEntriesForResponse(entries)
	return Page{
		Available: true, Entries: append([]Entry(nil), entries...),
		HasMore: hasMore, Total: metadata.Total, SourceCorrupt: corrupt,
	}, nil
}

func validOpenCodeSessionID(value string) bool {
	if !strings.HasPrefix(value, "ses_") || len(value) < 8 || len(value) > 128 {
		return false
	}
	for _, character := range value[4:] {
		switch {
		case character >= 'a' && character <= 'z', character >= 'A' && character <= 'Z', character >= '0' && character <= '9':
		default:
			return false
		}
	}
	return true
}

func parseOpenCodeRows(rows []openCodeRow) ([]Entry, bool) {
	entries, corrupt, _ := parseOpenCodeRowsContext(context.Background(), rows)
	return entries, corrupt
}

func parseOpenCodeRowsContext(ctx context.Context, rows []openCodeRow) (result []Entry, damaged bool, err error) {
	defer func() {
		if cancelled := ctx.Err(); cancelled != nil {
			result, damaged, err = nil, false, cancelled
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if len(rows) > 0 && rows[0].V2 {
		return parseOpenCodeV2RowsContext(ctx, rows)
	}
	entries := make([]Entry, 0)
	byMessage := make(map[string]int)
	corrupt := false
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		if row.MessageID == "" {
			continue
		}
		if len(row.MessageID) > 256 || strings.ContainsAny(row.MessageID, "\x00\r\n") {
			corrupt = true
			continue
		}
		index, exists := byMessage[row.MessageID]
		if !exists {
			var message struct {
				Role string `json:"role"`
			}
			if json.Unmarshal([]byte(row.MessageData), &message) != nil || (message.Role != "user" && message.Role != "assistant") {
				corrupt = true
				continue
			}
			entry := Entry{ID: row.MessageID, Role: message.Role}
			if row.Created > 0 {
				entry.Timestamp = time.UnixMilli(row.Created).UTC().Format(time.RFC3339Nano)
			}
			index = len(entries)
			byMessage[row.MessageID] = index
			entries = append(entries, entry)
		}
		if row.PartID == "" {
			continue
		}
		var part struct {
			Type   string `json:"type"`
			Text   string `json:"text"`
			Tool   string `json:"tool"`
			CallID string `json:"callID"`
			State  struct {
				Status   string          `json:"status"`
				Input    json.RawMessage `json:"input"`
				Output   string          `json:"output"`
				Error    string          `json:"error"`
				Metadata json.RawMessage `json:"metadata"`
			} `json:"state"`
		}
		if json.Unmarshal([]byte(row.PartData), &part) != nil {
			corrupt = true
			continue
		}
		entry := &entries[index]
		switch part.Type {
		case "text":
			text := sanitizeText(part.Text)
			if text == "" {
				continue
			}
			if entry.Text != "" {
				entry.Text += "\n"
			}
			entry.Text += text
		case "tool":
			input := ""
			if len(part.State.Input) > 0 && string(part.State.Input) != "null" {
				input = string(part.State.Input)
			}
			activity := newToolActivity(part.CallID, part.Tool, input)
			activity.Output, activity.Truncated = clampText(sanitizeText(part.State.Output), maxEntryBytes)
			activity.Error = part.State.Status == "error" || strings.TrimSpace(part.State.Error) != "" ||
				openCodeExitFailed(part.State.Metadata)
			entry.Tools = append(entry.Tools, activity)
		}
	}
	kept := entries[:0]
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		entry.Text, entry.Truncated = clampText(entry.Text, maxEntryBytes)
		if entry.Text != "" || len(entry.Tools) > 0 {
			kept = append(kept, entry)
		}
	}
	return kept, corrupt, nil
}

// parseOpenCodeV2Rows maps OpenCode 2.x session_message rows (one per turn) to
// entries. The role is the row type; assistant parts live in data.content.
func parseOpenCodeV2Rows(rows []openCodeRow) ([]Entry, bool) {
	entries, corrupt, _ := parseOpenCodeV2RowsContext(context.Background(), rows)
	return entries, corrupt
}

func parseOpenCodeV2RowsContext(ctx context.Context, rows []openCodeRow) (result []Entry, damaged bool, err error) {
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
		if row.MessageID == "" {
			continue
		}
		if len(row.MessageID) > 256 || strings.ContainsAny(row.MessageID, "\x00\r\n") {
			corrupt = true
			continue
		}
		if row.MessageType != "user" && row.MessageType != "assistant" {
			corrupt = true
			continue
		}
		var data struct {
			Text    string            `json:"text"`
			Content []json.RawMessage `json:"content"`
		}
		if json.Unmarshal([]byte(row.MessageData), &data) != nil {
			corrupt = true
			continue
		}
		entry := Entry{ID: row.MessageID, Role: row.MessageType}
		if row.Created > 0 {
			entry.Timestamp = time.UnixMilli(row.Created).UTC().Format(time.RFC3339Nano)
		}
		if row.MessageType == "user" {
			entry.Text = sanitizeText(data.Text)
		} else {
			itemCorrupt, err := parseOpenCodeV2ContentContext(ctx, &entry, data.Content)
			if err != nil {
				return nil, false, err
			}
			corrupt = corrupt || itemCorrupt
		}
		entry.Text, entry.Truncated = clampText(entry.Text, maxEntryBytes)
		if entry.Text != "" || len(entry.Tools) > 0 {
			entries = append(entries, entry)
		}
	}
	return entries, corrupt, nil
}

func parseOpenCodeV2Content(entry *Entry, items []json.RawMessage) bool {
	corrupt, _ := parseOpenCodeV2ContentContext(context.Background(), entry, items)
	return corrupt
}

// A cancelled helper must not expose partially attached content or tools.
func parseOpenCodeV2ContentContext(ctx context.Context, entry *Entry, items []json.RawMessage) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	candidate := *entry
	candidate.Tools = append([]ToolActivity(nil), entry.Tools...)
	corrupt, err := parseOpenCodeV2ContentInto(ctx, &candidate, items)
	if err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	*entry = candidate
	return corrupt, nil
}

func parseOpenCodeV2ContentInto(ctx context.Context, entry *Entry, items []json.RawMessage) (bool, error) {
	corrupt := false
	for _, raw := range items {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		var item struct {
			Type  string `json:"type"`
			Text  string `json:"text"`
			ID    string `json:"id"`
			Name  string `json:"name"`
			State struct {
				Status   string          `json:"status"`
				Input    json.RawMessage `json:"input"`
				Content  json.RawMessage `json:"content"`
				Output   json.RawMessage `json:"output"`
				Error    json.RawMessage `json:"error"`
				Metadata json.RawMessage `json:"metadata"`
			} `json:"state"`
		}
		if json.Unmarshal(raw, &item) != nil {
			corrupt = true
			continue
		}
		switch item.Type {
		case "text":
			text := sanitizeText(item.Text)
			if text == "" {
				continue
			}
			if entry.Text != "" {
				entry.Text += "\n"
			}
			entry.Text += text
		case "tool":
			input := ""
			if len(item.State.Input) > 0 && string(item.State.Input) != "null" {
				input = string(item.State.Input)
			}
			activity := newToolActivity(item.ID, item.Name, input)
			output, err := openCodeV2ToolOutputContext(ctx, item.State.Content, item.State.Output)
			if err != nil {
				return false, err
			}
			activity.Output, activity.Truncated = clampText(sanitizeText(output), maxEntryBytes)
			activity.Error = item.State.Status == "error" || openCodeV2ErrorSet(item.State.Error) ||
				openCodeExitFailed(item.State.Metadata)
			entry.Tools = append(entry.Tools, activity)
		}
	}
	return corrupt, ctx.Err()
}

// openCodeExitFailed reports whether a tool's state.metadata records a
// non-zero process exit code. OpenCode stores a failed shell command as
// "completed" with metadata.exit set, in both 1.x and 2.x. Metadata of an
// unexpected shape is ignored rather than treated as corrupt.
func openCodeExitFailed(metadata json.RawMessage) bool {
	var fields struct {
		Exit json.RawMessage `json:"exit"`
	}
	if json.Unmarshal(metadata, &fields) != nil {
		return false
	}
	var exit float64
	return json.Unmarshal(fields.Exit, &exit) == nil && exit != 0
}

// openCodeV2ToolOutput joins the text blocks of state.content, falling back to
// a string state.output. OpenCode appends separate blocks such as "Exited with
// code 3" after the command output, so blocks are separated by a newline when
// the previous one does not already end with one.
func openCodeV2ToolOutput(content, output json.RawMessage) string {
	text, _ := openCodeV2ToolOutputContext(context.Background(), content, output)
	return text
}

func openCodeV2ToolOutputContext(ctx context.Context, content, output json.RawMessage) (textResult string, err error) {
	defer func() {
		if cancelled := ctx.Err(); cancelled != nil {
			textResult, err = "", cancelled
		}
	}()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) == nil {
		var text strings.Builder
		for _, block := range blocks {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			if (block.Type != "text" && block.Type != "") || block.Text == "" {
				continue
			}
			if text.Len() > 0 && !strings.HasSuffix(text.String(), "\n") {
				text.WriteByte('\n')
			}
			text.WriteString(block.Text)
		}
		if text.Len() > 0 {
			return text.String(), nil
		}
	}
	var fallback string
	if json.Unmarshal(output, &fallback) == nil {
		return fallback, nil
	}
	return "", nil
}

// openCodeV2ErrorSet reports whether state.error holds a value of any JSON type.
func openCodeV2ErrorSet(raw json.RawMessage) bool {
	switch strings.TrimSpace(string(raw)) {
	case "", "null", "false", `""`, "{}", "[]":
		return false
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text) != ""
	}
	return true
}
