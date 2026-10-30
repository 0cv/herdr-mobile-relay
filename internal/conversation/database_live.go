package conversation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/sqliteexec"
)

const (
	databaseLiveRows   = 32768
	databaseLiveAppend = 512
	databaseLiveLoaded = 20000
	databaseLiveParts  = 256
)

type DatabaseWatch struct {
	mu              sync.Mutex
	browser         *Browser
	scope           BrowseScope
	path            string
	format          string
	revision        string
	watermark       int64
	latestTimestamp float64
	loaded          map[string][32]byte
	completed       map[string][32]byte
	started         bool
	suspended       bool
	pending         bool
}

type databaseLiveMeta struct {
	SessionID string `json:"session_id"`
	CWD       string `json:"cwd"`
	Schema    int    `json:"schema_version"`
}

type databaseLiveRow struct {
	RowID     int64   `json:"row_id"`
	SessionID string  `json:"session_id"`
	ID        string  `json:"id"`
	Timestamp float64 `json:"timestamp"`
}

func (b *Browser) NewDatabaseWatch(scope BrowseScope) *DatabaseWatch {
	if normalizedAgent(scope.Provider) != "opencode" && !isHermesAgent(scope.Provider) {
		return nil
	}
	return &DatabaseWatch{browser: b, scope: scope, loaded: make(map[string][32]byte), completed: make(map[string][32]byte)}
}

func databaseEntryHash(entry Entry) [32]byte {
	raw, _ := json.Marshal(entry)
	return sha256.Sum256(raw)
}

func (w *DatabaseWatch) NeedsReset() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.suspended && !w.pending
}

func databaseCompletedTools(entry Entry) [32]byte {
	ids := []string{}
	for _, tool := range entry.Tools {
		if tool.State == "completed" {
			ids = append(ids, toolAssociationID(tool))
		}
	}
	if len(ids) == 0 {
		return [32]byte{}
	}
	sort.Strings(ids)
	raw, _ := json.Marshal(ids)
	return sha256.Sum256(raw)
}

func (w *DatabaseWatch) Observe(page BrowsePage, replace bool) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.observe(page, replace)
	return w.suspended
}

func (w *DatabaseWatch) observe(page BrowsePage, replace bool) {
	if page.State != BrowseReady || !page.Available || page.SourceRevision != w.revision {
		return
	}
	seedTimestamp := !w.started || replace && (w.pending || w.suspended)
	if replace && (w.pending || w.suspended) {
		clear(w.loaded)
		clear(w.completed)
		w.latestTimestamp = 0
		w.suspended, w.pending = false, false
	}
	for _, entry := range page.Entries {
		if _, exists := w.loaded[entry.ID]; !exists && len(w.loaded) >= databaseLiveLoaded {
			w.suspended = true
			return
		}
		if _, exists := w.loaded[entry.ID]; !exists {
			w.loaded[entry.ID] = databaseEntryHash(entry)
			w.completed[entry.ID] = databaseCompletedTools(entry)
		}
		if stamp, err := time.Parse(time.RFC3339Nano, entry.Timestamp); seedTimestamp && err == nil {
			w.latestTimestamp = max(w.latestTimestamp, float64(stamp.UnixNano())/float64(time.Second))
		}
	}
	for _, id := range page.recordedPending {
		if _, exists := w.loaded[id]; exists {
			continue
		}
		if len(w.loaded) >= databaseLiveLoaded {
			w.suspended = true
			return
		}
		w.loaded[id] = [32]byte{}
	}
}

func (w *DatabaseWatch) executor() sqliteexec.Executor {
	if w.format == "hermes" {
		return w.browser.reader.hermes.queryExec()
	}
	return w.browser.reader.openCode.queryExec()
}

func (w *DatabaseWatch) query(ctx context.Context, query string, result any) string {
	ctx, cancel := context.WithTimeout(ctx, openCodeQueryTimeout)
	defer cancel()
	raw, code := runSQLiteJSON(ctx, w.executor(), w.path, query, maxOpenCodeOutput)
	if code != "" {
		return code
	}
	if json.Unmarshal(raw, result) != nil {
		return "source_corrupt"
	}
	return ""
}

func (w *DatabaseWatch) indexed(ctx context.Context, query, table, column string) bool {
	var plans []struct {
		Detail string `json:"detail"`
	}
	if w.query(ctx, "EXPLAIN QUERY PLAN "+query, &plans) != "" {
		return false
	}
	for _, plan := range plans {
		if strings.HasPrefix(plan.Detail, "SEARCH "+table+" ") && (strings.Contains(plan.Detail, "("+column+"=?)") || column == "id" && strings.Contains(plan.Detail, "(rowid=?)")) {
			return true
		}
	}
	return false
}

func databaseSQLText(value string) string {
	return "CAST(X'" + hex.EncodeToString([]byte(value)) + "' AS TEXT)"
}

func (w *DatabaseWatch) table() string {
	switch w.format {
	case "hermes":
		return "messages"
	case "v2":
		return "session_message"
	default:
		return "message"
	}
}

func (w *DatabaseWatch) highwater(ctx context.Context) (int64, string) {
	var rows []struct {
		ID int64 `json:"id"`
	}
	code := w.query(ctx, "SELECT rowid AS id FROM "+w.table()+" NOT INDEXED ORDER BY rowid DESC LIMIT 1", &rows)
	if len(rows) == 0 {
		return 0, code
	}
	return rows[0].ID, code
}

func (w *DatabaseWatch) source(ctx context.Context) string {
	var databases []string
	var code string
	hermes := isHermesAgent(w.scope.Provider)
	if hermes {
		if !safeSessionID(w.scope.SessionID) {
			return "invalid_session"
		}
		databases, code = w.browser.reader.hermes.databases()
	} else {
		if !validOpenCodeSessionID(w.scope.SessionID) {
			return "invalid_session"
		}
		databases, code = w.browser.reader.openCode.databases()
	}
	if code != "" {
		return code
	}
	for _, database := range databases {
		w.path = database
		formats := []string{"hermes"}
		if !hermes {
			stamp, ok := openCodeStamp(database)
			layout, layoutCode := w.browser.reader.openCode.layoutContext(ctx, database, stamp, ok)
			if layoutCode != "" {
				return layoutCode
			}
			formats = nil
			if layout.v2 {
				formats = append(formats, "v2")
			}
			if layout.v1 {
				formats = append(formats, "v1")
			}
		}
		for _, format := range formats {
			w.format = format
			table, cwd := "session", "directory"
			if format == "v2" {
				table = "session_v2"
			}
			if hermes {
				table, cwd = "sessions", "cwd"
			}
			query := fmt.Sprintf("SELECT id AS session_id,COALESCE(%s,'') AS cwd,(SELECT schema_version FROM pragma_schema_version) AS schema_version FROM %s WHERE id=%s LIMIT 1", cwd, table, databaseSQLText(w.scope.SessionID))
			if !w.indexed(ctx, "SELECT id FROM "+table+" WHERE id='live-plan' LIMIT 1", table, "id") {
				return "query_failed"
			}
			var metadata []databaseLiveMeta
			if code := w.query(ctx, query, &metadata); code != "" {
				return code
			}
			if len(metadata) == 0 {
				continue
			}
			if hermes && w.scope.CWD != "" && metadata[0].CWD != "" && !sameHermesDirectory(w.scope.CWD, metadata[0].CWD) {
				return "invalid_session"
			}
			if format == "v2" && len(formats) == 2 {
				query := "SELECT id FROM session_message WHERE session_id=" + databaseSQLText(w.scope.SessionID) + " LIMIT 1"
				var found []struct {
					ID string `json:"id"`
				}
				if !w.indexed(ctx, query, "session_message", "session_id") {
					return "query_failed"
				}
				if code := w.query(ctx, query, &found); code != "" {
					return code
				}
				if len(found) == 0 {
					continue
				}
			}
			w.revision = nativeDatabaseRevision(database, metadata[0].Schema, map[string]string{"v1": "false", "v2": "true", "hermes": "hermes"}[format])
			return ""
		}
	}
	return "invalid_session"
}

func (w *DatabaseWatch) Refresh(ctx context.Context) (result BrowsePage, resetRequired bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	defer func() {
		if resetRequired {
			w.pending = true
		}
	}()
	oldPath, oldFormat, oldRevision := w.path, w.format, w.revision
	if code := w.source(ctx); code != "" {
		return nativeFailure(code, "Recorded database updates require a fresh snapshot."), false
	}
	if !w.started {
		watermark, code := w.highwater(ctx)
		if code != "" {
			return nativeFailure(code, "Recorded database updates are unavailable."), false
		}
		w.watermark = watermark
		page, _ := w.browser.ReadPage(ctx, BrowseRequest{Scope: w.scope, Limit: maxPageSize})
		w.observe(page, true)
		w.started = true
		return page, page.SourceRevision != w.revision
	}
	page := BrowsePage{Available: true, State: BrowseReady, Mode: BrowseNative, SourceRevision: w.revision, Entries: []Entry{}}
	reset := oldPath != w.path || oldFormat != w.format || oldRevision != w.revision
	if w.suspended {
		return page, reset || !w.pending
	}
	high, code := w.highwater(ctx)
	if code != "" {
		return page, true
	}
	if high < w.watermark || oldPath != w.path || oldFormat != w.format {
		w.watermark = high
		reset = true
	}
	var appended []databaseLiveRow
	timestamp := "0"
	if w.format == "hermes" {
		timestamp = "COALESCE(timestamp,0)"
	}
	query := fmt.Sprintf("SELECT rowid AS row_id,CAST(id AS TEXT) AS id,session_id,%s AS timestamp FROM %s NOT INDEXED WHERE rowid>%d ORDER BY rowid LIMIT %d", timestamp, w.table(), w.watermark, databaseLiveAppend+1)
	if code := w.query(ctx, query, &appended); code != "" {
		return page, true
	}
	if len(appended) > databaseLiveAppend {
		w.watermark, _ = w.highwater(ctx)
		w.suspended = true
		return page, true
	}
	for _, row := range appended {
		w.watermark = row.RowID
		if row.SessionID == w.scope.SessionID {
			prior, observed := w.loaded[row.ID]
			if w.format == "hermes" && row.Timestamp <= w.latestTimestamp && (!observed || prior == ([32]byte{})) {
				w.watermark, _ = w.highwater(ctx)
				w.suspended = true
				return page, true
			}
			w.latestTimestamp = max(w.latestTimestamp, row.Timestamp)
			w.loaded[row.ID] = [32]byte{}
		}
	}
	if len(w.loaded) > databaseLiveLoaded {
		w.suspended = true
		return page, true
	}
	entries, pending, code := w.entries(ctx)
	if code != "" {
		w.suspended = true
		return page, true
	}
	seen := make(map[string]bool, len(entries)+len(pending))
	for _, id := range pending {
		if w.loaded[id] == ([32]byte{}) {
			seen[id] = true
		}
	}
	for _, entry := range entries {
		seen[entry.ID] = true
		hash := databaseEntryHash(entry)
		if prior := w.loaded[entry.ID]; prior != hash {
			page.Entries = append(page.Entries, entry)
			if prior != ([32]byte{}) {
				if page.RecordedEdits == nil {
					page.RecordedEdits = make(map[string]bool)
				}
				page.RecordedEdits[entry.ID] = true
			}
		}
		w.loaded[entry.ID] = hash
		w.completed[entry.ID] = databaseCompletedTools(entry)
	}
	for id, hash := range w.loaded {
		if !seen[id] {
			delete(w.loaded, id)
			delete(w.completed, id)
			if hash != ([32]byte{}) {
				return page, true
			}
		}
	}
	return page, reset
}

func (w *DatabaseWatch) entries(ctx context.Context) ([]Entry, []string, string) {
	if w.format == "hermes" {
		entries, code := w.hermesEntries(ctx)
		return entries, nil, code
	}
	if len(w.loaded) == 0 {
		return nil, nil, ""
	}
	ids := make([]string, 0, len(w.loaded))
	for id := range w.loaded {
		ids = append(ids, databaseSQLText(id))
	}
	sort.Strings(ids)
	table := w.table()
	if !w.indexed(ctx, "SELECT id FROM "+table+" WHERE id='live-plan' LIMIT 1", table, "id") {
		return nil, nil, "query_failed"
	}
	var rows []struct {
		openCodeRow
		Parts string `json:"parts"`
		Seq   int64  `json:"seq"`
	}
	parts, data, order := "'[]'", "data", "time_created,id"
	partLimit := min(databaseLiveParts+1, databaseLiveRows/len(w.loaded))
	if w.format == "v1" {
		if partLimit < 2 {
			return nil, nil, "output_limit"
		}
		partQuery := fmt.Sprintf("SELECT id,data FROM part WHERE message_id='live-plan' LIMIT %d", partLimit)
		if !w.indexed(ctx, partQuery, "part", "message_id") {
			return nil, nil, "query_failed"
		}
		parts = fmt.Sprintf("(SELECT json_group_array(json_object('part_id',id,'part_data',data)) FROM (SELECT id,data FROM part WHERE message_id=m.id LIMIT %d))", partLimit)
	} else {
		data, order = "data", "seq"
	}
	messageType, seq := "''", "0"
	if w.format == "v2" {
		messageType, seq = "type", "seq"
	}
	var indexes []struct {
		Name string `json:"name"`
	}
	if code := w.query(ctx, "SELECT name FROM pragma_index_list('"+table+"') WHERE origin='pk' LIMIT 1", &indexes); code != "" || len(indexes) != 1 {
		return nil, nil, "query_failed"
	}
	index := "\"" + strings.ReplaceAll(indexes[0].Name, "\"", "\"\"") + "\""
	query := fmt.Sprintf("SELECT id AS message_id,time_created,%s AS message_data,%s AS message_type,%s AS parts,%s AS seq FROM %s AS m INDEXED BY %s WHERE id IN (%s) AND session_id=%s ORDER BY %s LIMIT %d", data, messageType, parts, seq, table, index, strings.Join(ids, ","), databaseSQLText(w.scope.SessionID), order, databaseLiveLoaded+1)
	if !w.indexed(ctx, query, "m", "id") {
		return nil, nil, "query_failed"
	}
	if code := w.query(ctx, query, &rows); code != "" {
		return nil, nil, code
	}
	flat := make([]openCodeRow, 0, len(rows))
	for _, row := range rows {
		if w.format == "v2" {
			row.V2 = true
			if row.MessageType == "user" || row.MessageType == "assistant" {
				flat = append(flat, row.openCodeRow)
			}
			continue
		}
		var parts []struct {
			ID   string `json:"part_id"`
			Data string `json:"part_data"`
		}
		if json.Unmarshal([]byte(row.Parts), &parts) != nil {
			return nil, nil, "source_corrupt"
		}
		if len(parts) >= partLimit || len(flat)+len(parts) > databaseLiveRows {
			return nil, nil, "output_limit"
		}
		sort.Slice(parts, func(i, j int) bool { return parts[i].ID < parts[j].ID })
		if len(parts) == 0 {
			flat = append(flat, row.openCodeRow)
		}
		for _, part := range parts {
			item := row.openCodeRow
			item.PartID, item.PartData = part.ID, part.Data
			flat = append(flat, item)
		}
	}
	entries, corrupt := parseOpenCodeRows(flat)
	if corrupt {
		return nil, nil, "source_corrupt"
	}
	normalizeEntriesForResponse(entries)
	return entries, openCodePendingIDs(flat, entries), ""
}

func (w *DatabaseWatch) hermesSessionRangeIndexed(ctx context.Context, query string) bool {
	var plans []struct {
		Detail string `json:"detail"`
	}
	if w.query(ctx, "EXPLAIN QUERY PLAN "+query, &plans) != "" {
		return false
	}
	found := false
	for _, plan := range plans {
		if strings.Contains(plan.Detail, "TEMP B-TREE") {
			return false
		}
		if strings.HasPrefix(plan.Detail, "SEARCH messages ") && strings.Contains(plan.Detail, "(session_id=? AND rowid>?)") {
			found = true
		}
	}
	return found
}

func (w *DatabaseWatch) hermesEntries(ctx context.Context) ([]Entry, string) {
	if len(w.loaded) == 0 {
		return nil, ""
	}
	var minimum int64
	for id := range w.loaded {
		n, err := strconv.ParseInt(id, 10, 64)
		if err != nil || n <= 0 {
			return nil, "invalid_cursor"
		}
		if minimum == 0 || n < minimum {
			minimum = n
		}
	}
	var rows []struct {
		hermesRow
		Session   string `json:"session"`
		Active    int    `json:"active"`
		Compacted int    `json:"compacted"`
	}
	selection := fmt.Sprintf("SELECT id AS message_id,session_id AS session,role,hex(COALESCE(content,'')) AS content_hex,COALESCE(tool_call_id,'') AS tool_call_id,COALESCE(tool_calls,'') AS tool_calls,COALESCE(tool_name,'') AS tool_name,COALESCE(display_kind,'') AS display_kind,COALESCE(timestamp,0) AS timestamp,active,compacted FROM messages WHERE session_id=%s AND rowid>=%d ORDER BY rowid LIMIT %d", databaseSQLText(w.scope.SessionID), minimum, databaseLiveRows+1)
	query := selection
	if !w.hermesSessionRangeIndexed(ctx, selection) {
		query = fmt.Sprintf("SELECT id AS message_id,session_id AS session,role,hex(COALESCE(content,'')) AS content_hex,COALESCE(tool_call_id,'') AS tool_call_id,COALESCE(tool_calls,'') AS tool_calls,COALESCE(tool_name,'') AS tool_name,COALESCE(display_kind,'') AS display_kind,COALESCE(timestamp,0) AS timestamp,active,compacted FROM messages NOT INDEXED WHERE rowid>=%d ORDER BY rowid LIMIT %d", minimum, databaseLiveRows+1)
	}
	if code := w.query(ctx, query, &rows); code != "" {
		return nil, code
	}
	if len(rows) > databaseLiveRows {
		return nil, "output_limit"
	}
	type logicalRow struct {
		row    hermesRow
		active int
	}
	logical := make(map[[32]byte]logicalRow)
	for _, row := range rows {
		if row.Session != w.scope.SessionID || row.Active != 1 && row.Compacted != 1 || row.DisplayKind == "hidden" {
			continue
		}
		keyRow := row.hermesRow
		keyRow.MessageID, keyRow.DisplayKind = 0, ""
		raw, _ := json.Marshal(keyRow)
		key := sha256.Sum256(raw)
		prior, exists := logical[key]
		if !exists {
			logical[key] = logicalRow{row.hermesRow, row.Active}
			continue
		}
		if row.Active >= prior.active {
			id := prior.row.MessageID
			prior = logicalRow{row.hermesRow, row.Active}
			prior.row.MessageID = id
			logical[key] = prior
		}
	}
	selected := make([]hermesRow, 0, len(logical))
	toolRows := make([]hermesRow, 0)
	for _, item := range logical {
		row := item.row
		if row.Role == "tool" {
			toolRows = append(toolRows, row)
			continue
		}
		if _, loaded := w.loaded[strconv.FormatInt(row.MessageID, 10)]; loaded {
			selected = append(selected, row)
		}
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].MessageID < selected[j].MessageID })
	sort.Slice(toolRows, func(i, j int) bool { return toolRows[i].MessageID < toolRows[j].MessageID })
	tools := make([]hermesToolRow, 0, len(toolRows))
	for _, row := range toolRows {
		tools = append(tools, hermesToolRow{row.ToolCallID, row.ToolName, row.ContentHex})
	}
	entries, corrupt := parseHermesRows(selected)
	if attachHermesToolRows(entries, tools) {
		corrupt = true
	}
	for _, entry := range entries {
		if previous := w.completed[entry.ID]; previous != ([32]byte{}) && previous != databaseCompletedTools(entry) {
			return nil, "output_limit"
		}
	}
	if corrupt {
		return nil, "source_corrupt"
	}
	normalizeEntriesForResponse(entries)
	return entries, ""
}
