package conversation

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
	"github.com/0cv/herdr-mobile-relay/internal/sqliteexec"
)

const (
	openCodeQueryTimeout = 10 * time.Second
	maxOpenCodeOutput    = 8 * 1024 * 1024
	maxOpenCodeCache     = 128
)

type openCodeReader struct {
	home     string
	binary   string
	executor sqliteexec.Executor
	mu       sync.Mutex
	cache    map[string]openCodeCacheEntry
	layouts  map[string]openCodeLayoutEntry
}

type openCodeFileStamp struct {
	databaseSize   int64
	databaseTime   int64
	walSize        int64
	walTime        int64
	databaseID     string
	databaseChange string
	walID          string
	walChange      string
	walIndex       string
}

type openCodeCacheEntry struct {
	stamp   openCodeFileStamp
	rows    []openCodeRow
	hasMore bool
}

type openCodeRow struct {
	SchemaVersion int    `json:"schema_version"`
	SessionID     string `json:"session_id"`
	Directory     string `json:"directory"`
	Title         string `json:"title"`
	Updated       int64  `json:"time_updated"`
	Agent         string `json:"agent"`
	MessageID     string `json:"message_id"`
	Created       int64  `json:"time_created"`
	MessageData   string `json:"message_data"`
	PartID        string `json:"part_id"`
	PartData      string `json:"part_data"`
	Total         int    `json:"message_total"`
	CursorFound   int    `json:"cursor_found"`
	MessageType   string `json:"message_type"`
	HasMessages   int    `json:"has_messages"`
	Database      string `json:"-"`
	V2            bool   `json:"-"`

	pendingIDs []string
}

func newOpenCodeReader(home string) *openCodeReader {
	return &openCodeReader{home: home, binary: "sqlite3", executor: sqliteexec.MustFromEnv(), cache: make(map[string]openCodeCacheEntry), layouts: make(map[string]openCodeLayoutEntry)}
}

func (r *openCodeReader) queryExec() sqliteexec.Executor {
	return resolveQueryExecutor(r.executor, r.binary)
}

func (r *openCodeReader) databases() ([]string, string) {
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
		metadata.pendingIDs = openCodePendingIDs(rows, entries)
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
	stamp := openCodeFileStamp{databaseSize: info.Size(), databaseTime: info.ModTime().UnixNano(), databaseID: fileIdentity(info), databaseChange: fileChangeToken(info)}
	if info, err = os.Stat(database + "-wal"); err == nil {
		stamp.walSize = info.Size()
		stamp.walTime = info.ModTime().UnixNano()
		stamp.walID = fileIdentity(info)
		stamp.walChange = fileChangeToken(info)
	}
	stamp.walIndex = walIndexHeader(database)
	return stamp, true
}

func (r *openCodeReader) cachedQuery(key string, stamp openCodeFileStamp) ([]openCodeRow, bool, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.cache[key]
	if !ok || entry.stamp != stamp {
		return nil, false, false
	}
	return append([]openCodeRow(nil), entry.rows...), entry.hasMore, true
}

func (r *openCodeReader) storeQuery(key string, stamp openCodeFileStamp, rows []openCodeRow, hasMore bool) {
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
	var tables []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &tables); err != nil {
		return openCodeLayout{}, "source_corrupt"
	}
	present := make(map[string]bool, len(tables))
	for _, table := range tables {
		present[table.Name] = true
	}
	layout := openCodeLayout{
		v1: present["session"] && present["message"] && present["part"],
		v2: present["session_v2"] && present["session_message"],
	}
	if !layout.v1 && !layout.v2 {
		return openCodeLayout{}, "query_failed"
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
			`) SELECT (SELECT schema_version FROM pragma_schema_version) AS schema_version,s.id AS session_id,s.directory,s.title,s.time_updated,COALESCE(s.agent,'') AS agent,`+
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
	return r.runRows(ctx, database, query, limit, false)
}

// queryV2 reads the OpenCode 2.x tables. Only user and assistant turns are
// selected in SQL before LIMIT, so hidden turn types do not use page slots.
// Turn order is session_message.seq.
func (r *openCodeReader) queryV2(ctx context.Context, database, sessionID, before string, limit int) ([]openCodeRow, bool, string) {
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
			`) SELECT (SELECT schema_version FROM pragma_schema_version) AS schema_version,s.id AS session_id,COALESCE(s.directory,'') AS directory,COALESCE(s.title,'') AS title,`+
			`COALESCE(s.time_updated,0) AS time_updated,COALESCE(s.agent,'') AS agent,`+
			`COALESCE(sm.id,'') AS message_id,COALESCE(sm.type,'') AS message_type,COALESCE(sm.time_created,0) AS time_created,`+
			`COALESCE(sm.data,'null') AS message_data,`+
			`EXISTS(SELECT 1 FROM session_message WHERE session_id=s.id) AS has_messages,%s AS cursor_found `+
			`FROM session_v2 AS s LEFT JOIN selected AS sm ON 1=1 `+
			`WHERE s.id=CAST(X'%s' AS TEXT) ORDER BY sm.seq DESC;`,
		cursorCTE,
		sessionHex,
		visible,
		cursorFilter,
		limit+1,
		cursorFound,
		sessionHex,
	)
	return r.runRows(ctx, database, query, limit, true)
}

func (r *openCodeReader) runRows(ctx context.Context, database, query string, limit int, v2 bool) ([]openCodeRow, bool, string) {
	queryCtx, cancel := context.WithTimeout(ctx, openCodeQueryTimeout)
	defer cancel()
	raw, code := runSQLiteJSON(queryCtx, r.queryExec(), database, query, maxOpenCodeOutput)
	if code != "" {
		return nil, false, code
	}
	var rows []openCodeRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, false, "source_corrupt"
	}
	for left, right := 0, len(rows)-1; left < right; left, right = left+1, right-1 {
		rows[left], rows[right] = rows[right], rows[left]
	}
	for index := range rows {
		rows[index].V2 = v2
		if v2 {
			rows[index].Total = UnknownTotal
		}
	}
	messageIDs := make([]string, 0, limit+1)
	seen := make(map[string]struct{}, limit+1)
	for _, row := range rows {
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
			if row.MessageID != extraID {
				kept = append(kept, row)
			}
		}
		rows = kept
	}
	return rows, hasMore, ""
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

func openCodePendingIDs(rows []openCodeRow, entries []Entry) []string {
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		seen[entry.ID] = true
	}
	var pending []string
	for _, row := range rows {
		if seen[row.MessageID] || row.MessageID == "" || len(row.MessageID) > 256 || strings.ContainsAny(row.MessageID, "\x00\r\n") {
			continue
		}
		role := row.MessageType
		if !row.V2 {
			var message struct {
				Role string `json:"role"`
			}
			if json.Unmarshal([]byte(row.MessageData), &message) != nil {
				continue
			}
			role = message.Role
		}
		if role != "user" && role != "assistant" {
			continue
		}
		seen[row.MessageID] = true
		pending = append(pending, row.MessageID)
	}
	return pending
}

func parseOpenCodeRows(rows []openCodeRow) ([]Entry, bool) {
	if len(rows) > 0 && rows[0].V2 {
		return parseOpenCodeV2Rows(rows)
	}
	entries := make([]Entry, 0)
	byMessage := make(map[string]int)
	corrupt := false
	for _, row := range rows {
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
			activity.State = openCodeToolState(part.State.Status)
			activity.Output, activity.Truncated = clampText(sanitizeText(part.State.Output), maxEntryBytes)
			activity.Error = part.State.Status == "error" || strings.TrimSpace(part.State.Error) != "" ||
				openCodeExitFailed(part.State.Metadata)
			if activity.Error {
				activity.State = "failed"
			}
			entry.Tools = append(entry.Tools, activity)
		}
	}
	kept := entries[:0]
	for _, entry := range entries {
		entry.Text, entry.Truncated = clampText(entry.Text, maxEntryBytes)
		if entry.Text != "" || len(entry.Tools) > 0 {
			kept = append(kept, entry)
		}
	}
	return kept, corrupt
}

// parseOpenCodeV2Rows maps OpenCode 2.x session_message rows (one per turn) to
// entries. The role is the row type; assistant parts live in data.content.
func parseOpenCodeV2Rows(rows []openCodeRow) ([]Entry, bool) {
	entries := make([]Entry, 0, len(rows))
	corrupt := false
	for _, row := range rows {
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
		} else if itemCorrupt := parseOpenCodeV2Content(&entry, data.Content); itemCorrupt {
			corrupt = true
		}
		entry.Text, entry.Truncated = clampText(entry.Text, maxEntryBytes)
		if entry.Text != "" || len(entry.Tools) > 0 {
			entries = append(entries, entry)
		}
	}
	return entries, corrupt
}

func parseOpenCodeV2Content(entry *Entry, items []json.RawMessage) bool {
	corrupt := false
	for _, raw := range items {
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
			activity.State = openCodeToolState(item.State.Status)
			activity.Output, activity.Truncated = clampText(sanitizeText(openCodeV2ToolOutput(item.State.Content, item.State.Output)), maxEntryBytes)
			activity.Error = item.State.Status == "error" || openCodeV2ErrorSet(item.State.Error) ||
				openCodeExitFailed(item.State.Metadata)
			if activity.Error {
				activity.State = "failed"
			}
			entry.Tools = append(entry.Tools, activity)
		}
	}
	return corrupt
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
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) == nil {
		var text strings.Builder
		for _, block := range blocks {
			if (block.Type != "text" && block.Type != "") || block.Text == "" {
				continue
			}
			if text.Len() > 0 && !strings.HasSuffix(text.String(), "\n") {
				text.WriteByte('\n')
			}
			text.WriteString(block.Text)
		}
		if text.Len() > 0 {
			return text.String()
		}
	}
	var fallback string
	if json.Unmarshal(output, &fallback) == nil {
		return fallback
	}
	return ""
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
