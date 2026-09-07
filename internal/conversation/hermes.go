package conversation

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
)

const (
	hermesQueryTimeout = 3 * time.Second
	maxHermesOutput    = 8 * 1024 * 1024
)

type hermesReader struct {
	home   string
	binary string
}

type hermesRow struct {
	SessionID   string  `json:"session_id"`
	CWD         string  `json:"cwd"`
	Title       string  `json:"title"`
	MessageID   int64   `json:"message_id"`
	Role        string  `json:"role"`
	ContentHex  string  `json:"content_hex"`
	ToolCallID  string  `json:"tool_call_id"`
	ToolCalls   string  `json:"tool_calls"`
	ToolName    string  `json:"tool_name"`
	DisplayKind string  `json:"display_kind"`
	Timestamp   float64 `json:"timestamp"`
	Total       int     `json:"message_total"`
	CursorFound int     `json:"cursor_found"`
}

type hermesToolRow struct {
	ToolCallID string `json:"tool_call_id"`
	ToolName   string `json:"tool_name"`
	ContentHex string `json:"content_hex"`
}

func newHermesReader(home string) *hermesReader {
	return &hermesReader{home: home, binary: "sqlite3"}
}

func (r *hermesReader) databases() ([]string, string) {
	if _, err := exec.LookPath(r.binary); err != nil {
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
	if !safeSessionID(sessionID) {
		return Location{}
	}
	databases, code := r.databases()
	if code != "" {
		return Location{}
	}
	for _, database := range databases {
		rows, _, queryCode := r.query(database, sessionID, "", 1)
		if queryCode != "" || len(rows) == 0 || rows[0].SessionID != sessionID {
			continue
		}
		if cwd != "" && rows[0].CWD != "" && !sameOpenCodeDirectory(cwd, rows[0].CWD) {
			return Location{}
		}
		return Location{Path: database, Root: filepath.Dir(database), Title: rows[0].Title}
	}
	return Location{}
}

func (r *hermesReader) readFrom(database, sessionID, before string, limit int) ([]Entry, bool, bool, hermesRow, string) {
	if !safeSessionID(sessionID) {
		return nil, false, false, hermesRow{}, "invalid_session"
	}
	if before != "" {
		cursor, err := strconv.ParseInt(before, 10, 64)
		if err != nil || cursor <= 0 {
			return nil, false, false, hermesRow{}, "invalid_cursor"
		}
	}
	rows, hasMore, queryCode := r.query(database, sessionID, before, limit)
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
	if len(entries) == 0 && rows[0].Total > 0 && corrupt {
		return nil, false, true, rows[0], "source_corrupt"
	}
	if toolCorrupt := r.attachToolResults(database, sessionID, entries); toolCorrupt {
		corrupt = true
	}
	return entries, hasMore, corrupt, rows[0], ""
}

func (r *hermesReader) query(database, sessionID, before string, limit int) ([]hermesRow, bool, string) {
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
			return nil, false, "invalid_cursor"
		}
		// Resolve an old physical row, including an archived compaction
		// generation, to its stable logical message before selecting rows.
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
			`SELECT s.id AS session_id,COALESCE(s.cwd,'') AS cwd,COALESCE(s.title,'') AS title,`+
			`COALESCE(sm.logical_id,0) AS message_id,COALESCE(sm.role,'') AS role,`+
			`hex(COALESCE(sm.content,'')) AS content_hex,COALESCE(sm.tool_call_id,'') AS tool_call_id,`+
			`COALESCE(sm.tool_calls,'') AS tool_calls,COALESCE(sm.tool_name,'') AS tool_name,`+
			`COALESCE(sm.display_kind,'') AS display_kind,COALESCE(sm.logical_timestamp,0) AS timestamp,`+
			`(SELECT COUNT(*) FROM displayed) AS message_total,%s AS cursor_found `+
			`FROM sessions AS s LEFT JOIN selected AS sm ON 1=1 `+
			`WHERE s.id=CAST(X'%s' AS TEXT) ORDER BY sm.logical_id DESC;`,
		sessionHex, limit+1, cursorFound, sessionHex,
	)
	ctx, cancel := context.WithTimeout(context.Background(), hermesQueryTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, r.binary, "-readonly", "-batch", "-json", database, query)
	stdout := &boundedBuffer{remaining: maxHermesOutput}
	var stderr boundedBuffer
	stderr.remaining = 4096
	command.Stdout = stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if stdout.overflow {
			return nil, false, "output_limit"
		}
		return nil, false, "query_failed"
	}
	var rows []hermesRow
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
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
	for left, right := 0, len(rows)-1; left < right; left, right = left+1, right-1 {
		rows[left], rows[right] = rows[right], rows[left]
	}
	return rows, hasMore, ""
}

func (r *hermesReader) attachToolResults(database, sessionID string, entries []Entry) bool {
	callIDs := make(map[string]struct{})
	for _, entry := range entries {
		for _, tool := range entry.Tools {
			if tool.ID != "" {
				callIDs[tool.ID] = struct{}{}
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
	rows, code := r.queryToolRows(database, sessionID, ids)
	if code != "" {
		return true
	}
	for _, row := range rows {
		output, truncated := clampText(sanitizeText(hermesTextHex(row.ContentHex)), maxEntryBytes)
		if output == "" {
			continue
		}
		for entryIndex := range entries {
			for toolIndex := range entries[entryIndex].Tools {
				tool := &entries[entryIndex].Tools[toolIndex]
				if tool.ID != row.ToolCallID {
					continue
				}
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
	return false
}

func (r *hermesReader) queryToolRows(database, sessionID string, callIDs []string) ([]hermesToolRow, string) {
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
	ctx, cancel := context.WithTimeout(context.Background(), hermesQueryTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, r.binary, "-readonly", "-batch", "-json", database, query)
	stdout := &boundedBuffer{remaining: maxHermesOutput}
	var stderr boundedBuffer
	stderr.remaining = 4096
	command.Stdout = stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if stdout.overflow {
			return nil, "output_limit"
		}
		return nil, "query_failed"
	}
	var rows []hermesToolRow
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
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
		corrupt = corrupt || toolsCorrupt
		text := sanitizeText(hermesTextHex(row.ContentHex))
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
	var decoded any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return nil, true
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
		return nil, true
	}
	activities := make([]ToolActivity, 0, len(calls))
	corrupt := false
	for _, rawCall := range calls {
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
		activities = append(activities, newToolActivity(id, name, input))
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

func hermesTextHex(encoded string) string {
	if encoded == "" {
		return ""
	}
	decoded, err := hex.DecodeString(encoded)
	if err != nil {
		return ""
	}
	return hermesText(string(decoded))
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
	if cwd != "" && metadata.CWD != "" && !sameOpenCodeDirectory(cwd, metadata.CWD) {
		return unavailableCode("invalid_session", "This conversation belongs to a different workspace."), nil
	}
	return Page{
		Available:     true,
		Entries:       append([]Entry(nil), entries...),
		HasMore:       hasMore,
		Total:         metadata.Total,
		SourceCorrupt: corrupt,
	}, nil
}
