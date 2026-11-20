package conversation

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/agentkind"
	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
	"github.com/0cv/herdr-mobile-relay/internal/sqliteexec"
)

const (
	hermesQueryTimeout = 10 * time.Second
	maxHermesOutput    = 8 * 1024 * 1024
)

type hermesReader struct {
	home     string
	executor sqliteexec.Executor
}

// Rows that agree on these columns are one logical message. The SQL page
// query and the live feed's hermesRow.logicalKey must name the same fields.
const hermesLogicalColumns = "role,content,tool_call_id,tool_calls,tool_name,logical_timestamp"

type hermesLogicalKey struct {
	Role       string
	ContentHex string
	ToolCallID string
	ToolCalls  string
	ToolName   string
	Timestamp  float64
}

func (r hermesRow) logicalKey() hermesLogicalKey {
	return hermesLogicalKey{r.Role, r.ContentHex, r.ToolCallID, r.ToolCalls, r.ToolName, r.Timestamp}
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
	return &hermesReader{home: home, executor: sqliteexec.New()}
}

func (r *hermesReader) databases() ([]string, string) {
	return containedDatabases(agentroots.HermesData(r.home), agentroots.HermesDBs(r.home))
}

func (r *hermesReader) locate(cwd, sessionID string) Location {
	location, _ := r.locateContext(context.Background(), cwd, sessionID)
	return location
}

func (r *hermesReader) locateContext(ctx context.Context, cwd, sessionID string) (Location, string) {
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

func (r *hermesReader) readFromContext(ctx context.Context, database, sessionID, before string, limit int) nativeReadResult[hermesRow] {
	if !safeSessionID(sessionID) {
		return nativeReadResult[hermesRow]{Code: "invalid_session"}
	}
	if before != "" {
		cursor, err := strconv.ParseInt(before, 10, 64)
		if err != nil || cursor <= 0 {
			return nativeReadResult[hermesRow]{Code: "invalid_cursor"}
		}
	}
	rows, hasMore, queryCode := r.queryContext(ctx, database, sessionID, before, limit)
	if queryCode != "" {
		return nativeReadResult[hermesRow]{Code: queryCode}
	}
	if len(rows) == 0 || rows[0].SessionID != sessionID {
		return nativeReadResult[hermesRow]{Code: "invalid_session"}
	}
	if before != "" && rows[0].CursorFound == 0 {
		return nativeReadResult[hermesRow]{Code: "invalid_cursor"}
	}
	entries, corrupt := parseHermesRows(rows)
	if toolCorrupt := r.attachToolResultsContext(ctx, database, sessionID, entries); toolCorrupt {
		corrupt = true
	}
	return nativeReadResult[hermesRow]{Entries: entries, HasMore: hasMore, Corrupt: corrupt, Metadata: rows[0]}
}

func (r *hermesReader) queryContext(ctx context.Context, database, sessionID, before string, limit int) ([]hermesRow, bool, string) {
	limit = clampPageLimit(limit)
	sessionHex := hex.EncodeToString([]byte(sessionID))
	cursorCTE := "cursor AS (SELECT NULL AS logical_id WHERE 0)"
	cursorFilter := ""
	cursorFound := "1"
	if before != "" {
		cursor, err := strconv.ParseInt(before, 10, 64)
		if err != nil || cursor <= 0 {
			return nil, false, "invalid_cursor"
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
			`PARTITION BY `+hermesLogicalColumns+
			`) AS logical_id,`+
			`ROW_NUMBER() OVER (`+
			`PARTITION BY `+hermesLogicalColumns+` `+
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
	queryCtx, cancel := context.WithTimeout(ctx, hermesQueryTimeout)
	defer cancel()
	raw, code := runSQLiteJSON(queryCtx, r.executor, database, query, maxHermesOutput)
	if code != "" {
		return nil, false, code
	}
	var rows []hermesRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, false, "source_corrupt"
	}
	anchorCount := 0
	for _, row := range rows {
		if row.MessageID > 0 {
			anchorCount++
		}
	}
	hasMore := anchorCount > limit
	if hasMore && len(rows) > 0 {
		rows = rows[:len(rows)-1]
	}
	slices.Reverse(rows)
	return rows, hasMore, ""
}

func (r *hermesReader) attachToolResultsContext(ctx context.Context, database, sessionID string, entries []Entry) bool {
	callIDs := make(map[string]struct{})
	for _, entry := range entries {
		for _, tool := range entry.Tools {
			if id := toolAssociationID(tool); id != "" {
				callIDs[id] = struct{}{}
			}
		}
	}
	if len(callIDs) == 0 {
		return false
	}
	ids := make([]string, 0, len(callIDs))
	for id := range callIDs {
		ids = append(ids, id)
	}
	rows, code := r.queryToolRowsContext(ctx, database, sessionID, ids)
	if code != "" {
		return true
	}
	return attachHermesToolRows(entries, rows)
}

func attachHermesToolRows(entries []Entry, rows []hermesToolRow) bool {
	corrupt := false
	for _, row := range rows {
		row.ToolCallID = strings.TrimSpace(row.ToolCallID)
		outputText, outputCorrupt := hermesTextHexValue(row.ContentHex)
		if outputCorrupt {
			corrupt = true
			continue
		}
		output, truncated := clampText(sanitizeText(outputText), maxEntryBytes)
		for entryIndex := range entries {
			for toolIndex := range entries[entryIndex].Tools {
				tool := &entries[entryIndex].Tools[toolIndex]
				if toolAssociationID(*tool) != row.ToolCallID {
					continue
				}
				tool.State = "completed"
				if tool.Output != "" && tool.Output != output {
					var combinedTruncated bool
					tool.Output, combinedTruncated = clampText(tool.Output+"\n"+output, maxEntryBytes)
					tool.Truncated = tool.Truncated || combinedTruncated
				} else if tool.Output == "" {
					tool.Output = output
				}
				tool.Truncated = tool.Truncated || truncated
			}
		}
	}
	return corrupt
}

func (r *hermesReader) queryToolRowsContext(ctx context.Context, database, sessionID string, callIDs []string) ([]hermesToolRow, string) {
	sessionHex := hex.EncodeToString([]byte(sessionID))
	encodedIDs := make([]string, 0, len(callIDs))
	for _, id := range callIDs {
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
	queryCtx, cancel := context.WithTimeout(ctx, hermesQueryTimeout)
	defer cancel()
	raw, code := runSQLiteJSON(queryCtx, r.executor, database, query, maxHermesOutput)
	if code != "" {
		return nil, code
	}
	var rows []hermesToolRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, "source_corrupt"
	}
	return rows, ""
}

func parseHermesRows(rows []hermesRow) ([]Entry, bool) {
	entries := make([]Entry, 0, len(rows))
	corrupt := false
	for _, row := range rows {
		if row.MessageID <= 0 || (row.Role != "user" && row.Role != "assistant") {
			continue
		}
		tools, toolsCorrupt := parseHermesToolCalls(row.ToolCalls)
		textValue, textCorrupt := hermesTextHexValue(row.ContentHex)
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
	return entries, corrupt
}

func parseHermesToolCalls(raw string) ([]ToolActivity, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return nil, false
	}
	var calls []json.RawMessage
	switch raw[0] {
	case '[':
		if json.Unmarshal([]byte(raw), &calls) != nil {
			return nil, true
		}
	case '{':
		call := decodeRawJSONFields([]byte(raw))
		if call == nil {
			return nil, true
		}
		if nested := call["tool_calls"]; len(nested) > 0 && nested[0] == '[' {
			_ = json.Unmarshal(nested, &calls)
		} else {
			calls = []json.RawMessage{json.RawMessage(raw)}
		}
	default:
		return nil, true
	}
	activities := make([]ToolActivity, 0, len(calls))
	corrupt := false
	for _, rawCall := range calls {
		call := decodeRawJSONFields(rawCall)
		if call == nil {
			corrupt = true
			continue
		}
		function := decodeRawJSONFields(call["function"])
		id := call.firstString("id", "call_id", "tool_call_id")
		name := call.firstString("name", "tool_name")
		args := call.firstValue("arguments", "input")
		if id == "" {
			id = function.firstString("id", "call_id", "tool_call_id")
		}
		if name == "" {
			name = function.firstString("name", "tool_name")
		}
		if args == nil {
			args = function.firstValue("arguments", "input")
		}
		var input any
		if args != nil && json.Unmarshal(args, &input) != nil {
			corrupt = true
			continue
		}
		activity := newToolActivity(id, name, input)
		if !activity.Truncated && editInputFor(agentkind.Hermes, activity.Name) != (editInput{}) {
			activity.ambiguousEditInput = !validEditToolJSON(rawCall)
		}
		activity.State = "running"
		activities = append(activities, activity)
	}
	return activities, corrupt
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

func hermesTextHexValue(encoded string) (string, bool) {
	if encoded == "" {
		return "", false
	}
	decoded, err := hex.DecodeString(encoded)
	if err != nil {
		return "", true
	}
	return hermesText(string(decoded)), false
}

func hermesTimestamp(value float64) string {
	if value <= 0 {
		return ""
	}
	return time.Unix(0, int64(value*float64(time.Second))).UTC().Format(time.RFC3339Nano)
}

func (r *Reader) readHermesFor(agent, cwd, sessionID string, limit int) ([]Entry, error) {
	sessionID = strings.TrimSpace(sessionID)
	if !safeSessionID(sessionID) {
		return nil, ErrUnavailable
	}
	limit = clampPageLimit(limit)
	location := r.LocateWithProject(agent, ProjectContext{CWD: cwd}, sessionID)
	if location.Path == "" {
		return nil, ErrUnavailable
	}
	result := r.hermes.readFromContext(context.Background(), location.Path, sessionID, "", limit)
	if result.Code != "" {
		return nil, ErrUnavailable
	}
	if cwd != "" && result.Metadata.CWD != "" && !sameHermesDirectory(cwd, result.Metadata.CWD) {
		return nil, ErrUnavailable
	}
	normalizeEntriesForResponse(result.Entries)
	return append([]Entry(nil), result.Entries...), nil
}
