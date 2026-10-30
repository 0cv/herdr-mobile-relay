package conversation

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
	"github.com/0cv/herdr-mobile-relay/internal/sqliteexec"
)

// Antigravity CLI (agy) keeps one SQLite database per conversation at
// <data root>/conversations/<conversation-id>.db. The `steps` table holds one
// row per step with a protobuf payload:
//
//	step_type 14: user message      19.2 = text
//	step_type 15: assistant step    20.1 = reply text, 20.7 = tool call
//	                                (7.1 id, 7.2 name, 7.3 JSON arguments)
//	step_type 132: tool result      metadata 4.1 = tool call id
//
// Other step types (planner state, summaries) are not part of the transcript.
const (
	agyQueryTimeout = 10 * time.Second
	maxAgyOutput    = 8 * 1024 * 1024

	agyStepUser      = 14
	agyStepAssistant = 15
	agyStepToolDone  = 132
	// agyToolLookahead bounds how far past a page's last step a tool result is
	// searched for; results follow their call within a few steps.
	agyToolLookahead = 256
)

type agyReader struct {
	home     string
	binary   string
	executor sqliteexec.Executor
}

type agyRow struct {
	SchemaVersion int    `json:"schema_version"`
	Idx           int64  `json:"idx"`
	StepType      int    `json:"step_type"`
	PayloadHex    string `json:"payload_hex"`
	MetadataHex   string `json:"metadata_hex"`
	HasMore       int    `json:"has_more"`
}

type agyPageMeta struct {
	SchemaVersion int
	Total         int
	OldestIdx     int64
}

func newAgyReader(home string) *agyReader {
	return &agyReader{home: home, binary: "sqlite3", executor: sqliteexec.MustFromEnv()}
}

func (r *agyReader) queryExec() sqliteexec.Executor {
	return resolveQueryExecutor(r.executor, r.binary)
}

func isAgyAgent(agent string) bool {
	switch normalizedAgent(agent) {
	case "agy", "antigravity", "antigravitycli":
		return true
	default:
		return false
	}
}

func (r *agyReader) locate(sessionID string) Location {
	location, _ := r.locateContext(sessionID)
	return location
}

func (r *agyReader) locateContext(sessionID string) (Location, string) {
	if !canonicalSessionID.MatchString(sessionID) {
		return Location{}, "invalid_session"
	}
	if exec := r.queryExec(); exec == nil || !exec.Ready() {
		return Location{}, "source_unavailable"
	}
	for _, root := range agentroots.Agy(r.home) {
		if path := containedRegularFile(filepath.Join(root, sessionID+".db"), root); path != "" {
			return Location{Path: path, Root: root}, ""
		}
	}
	return Location{}, "invalid_session"
}

// databasePaths lists the databases a change token must cover for sessionID.
func (r *agyReader) databasePaths(sessionID string) []string {
	if !canonicalSessionID.MatchString(sessionID) {
		return nil
	}
	var paths []string
	for _, root := range agentroots.Agy(r.home) {
		if path := containedRegularFile(filepath.Join(root, sessionID+".db"), root); path != "" {
			paths = append(paths, path)
		}
	}
	return paths
}

func (r *agyReader) readFromContext(ctx context.Context, database, before string, limit int) ([]Entry, bool, bool, agyPageMeta, string) {
	var cursor int64 = -1
	if before != "" {
		value, err := strconv.ParseInt(before, 10, 64)
		if err != nil || value < 0 {
			return nil, false, false, agyPageMeta{}, "invalid_cursor"
		}
		cursor = value
	}
	if limit < 1 {
		limit = defaultPageSize
	}
	if limit > maxPageSize {
		limit = maxPageSize
	}
	cursorFilter := ""
	if cursor >= 0 {
		cursorFilter = fmt.Sprintf(" AND idx < %d", cursor)
	}
	query := fmt.Sprintf(
		`SELECT (SELECT schema_version FROM pragma_schema_version) AS schema_version,`+
			`idx,step_type,hex(step_payload) AS payload_hex,'' AS metadata_hex,0 AS has_more `+
			`FROM steps WHERE step_type IN (%d,%d)%s ORDER BY idx DESC LIMIT %d;`,
		agyStepUser, agyStepAssistant, cursorFilter, limit+1,
	)
	rows, code := r.queryRows(ctx, database, query)
	if code != "" {
		return nil, false, false, agyPageMeta{}, code
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	meta := agyPageMeta{Total: UnknownTotal}
	if len(rows) > 0 {
		meta.SchemaVersion = rows[0].SchemaVersion
		meta.OldestIdx = rows[len(rows)-1].Idx
	}
	for left, right := 0, len(rows)-1; left < right; left, right = left+1, right-1 {
		rows[left], rows[right] = rows[right], rows[left]
	}
	entries, corrupt := parseAgyRows(rows)
	if len(rows) > 0 && len(entries) > 0 {
		if toolCorrupt := r.attachToolResults(ctx, database, rows[0].Idx, rows[len(rows)-1].Idx, entries); toolCorrupt {
			corrupt = true
		}
	}
	return entries, hasMore, corrupt, meta, ""
}

func (r *agyReader) queryRows(ctx context.Context, database, query string) ([]agyRow, string) {
	queryCtx, cancel := context.WithTimeout(ctx, agyQueryTimeout)
	defer cancel()
	raw, code := runSQLiteJSON(queryCtx, r.queryExec(), database, query, maxAgyOutput)
	if code != "" {
		return nil, code
	}
	var rows []agyRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, "source_corrupt"
	}
	return rows, ""
}

// attachToolResults marks each tool call completed once a tool-result step
// carrying its id exists. Result bodies live in per-step files outside the
// database, which the relay deliberately does not open.
func (r *agyReader) attachToolResults(ctx context.Context, database string, lo, hi int64, entries []Entry) bool {
	pending := make(map[string]struct{})
	for _, entry := range entries {
		for _, tool := range entry.Tools {
			if id := toolAssociationID(tool); id != "" {
				pending[id] = struct{}{}
			}
		}
	}
	if len(pending) == 0 {
		return false
	}
	query := fmt.Sprintf(
		`SELECT idx,step_type,'' AS payload_hex,hex(metadata) AS metadata_hex `+
			`FROM steps WHERE step_type=%d AND idx>%d AND idx<=%d ORDER BY idx;`,
		agyStepToolDone, lo, hi+agyToolLookahead,
	)
	rows, code := r.queryRows(ctx, database, query)
	if code != "" {
		return true
	}
	corrupt := false
	done := make(map[string]bool, len(rows))
	for _, row := range rows {
		raw, err := hex.DecodeString(row.MetadataHex)
		if err != nil {
			corrupt = true
			continue
		}
		fields, ok := parsePB(raw)
		if !ok {
			corrupt = true
			continue
		}
		if id, ok := pbPath(fields, 4, 1); ok {
			done[string(id)] = true
		}
	}
	for entryIndex := range entries {
		for toolIndex := range entries[entryIndex].Tools {
			tool := &entries[entryIndex].Tools[toolIndex]
			if done[toolAssociationID(*tool)] {
				tool.State = "completed"
			}
		}
	}
	return corrupt
}

func parseAgyRows(rows []agyRow) ([]Entry, bool) {
	entries := make([]Entry, 0, len(rows))
	corrupt := false
	for _, row := range rows {
		raw, err := hex.DecodeString(row.PayloadHex)
		if err != nil {
			corrupt = true
			continue
		}
		step, ok := parsePB(raw)
		if !ok {
			corrupt = true
			continue
		}
		var entry Entry
		var stepCorrupt bool
		switch row.StepType {
		case agyStepUser:
			entry, stepCorrupt = parseAgyUser(step)
		case agyStepAssistant:
			entry, stepCorrupt = parseAgyAssistant(step)
		default:
			continue
		}
		corrupt = corrupt || stepCorrupt
		if entry.Text == "" && len(entry.Tools) == 0 {
			continue
		}
		entry.ID = strconv.FormatInt(row.Idx, 10)
		entry.Timestamp = agyTimestamp(step)
		entries = append(entries, entry)
	}
	return entries, corrupt
}

func parseAgyUser(step []pbField) (Entry, bool) {
	text, _ := pbPath(step, 19, 2)
	body, truncated := clampText(sanitizeText(agyString(text)), maxEntryBytes)
	return Entry{Role: "user", Text: body, Truncated: truncated}, false
}

func parseAgyAssistant(step []pbField) (Entry, bool) {
	message, ok := pbBytes(step, 20)
	if !ok {
		return Entry{Role: "assistant"}, false
	}
	fields, ok := parsePB(message)
	if !ok {
		return Entry{Role: "assistant"}, true
	}
	text, _ := pbBytes(fields, 1)
	body, truncated := clampText(sanitizeText(agyString(text)), maxEntryBytes)
	entry := Entry{Role: "assistant", Text: body, Truncated: truncated}
	corrupt := false
	for _, call := range pbAll(fields, 7) {
		callFields, ok := parsePB(call)
		if !ok {
			corrupt = true
			continue
		}
		id, _ := pbBytes(callFields, 1)
		name, _ := pbBytes(callFields, 2)
		args, _ := pbBytes(callFields, 3)
		var input any = agyString(args)
		var decoded any
		if json.Unmarshal(args, &decoded) == nil {
			input = decoded
		}
		tool := newToolActivity(agyString(id), agyString(name), input)
		tool.State = "running"
		entry.Tools = append(entry.Tools, tool)
	}
	return entry, corrupt
}

func agyString(data []byte) string {
	if !utf8.Valid(data) {
		return strings.ToValidUTF8(string(data), "\uFFFD")
	}
	return string(data)
}

func agyTimestamp(step []pbField) string {
	stamp, ok := pbPath(step, 5, 1)
	if !ok {
		return ""
	}
	fields, ok := parsePB(stamp)
	if !ok {
		return ""
	}
	var seconds, nanos uint64
	for _, field := range fields {
		switch {
		case field.num == 1 && !field.isData:
			seconds = field.varint
		case field.num == 2 && !field.isData:
			nanos = field.varint
		}
	}
	if seconds == 0 {
		return ""
	}
	return time.Unix(int64(seconds), int64(nanos)).UTC().Format(time.RFC3339Nano)
}

func (r *Reader) readAgyFor(sessionID, before string, limit int) (Page, error) {
	sessionID = strings.TrimSpace(sessionID)
	if !canonicalSessionID.MatchString(sessionID) {
		return unavailableCode("invalid_session", "This agent has not reported a conversation session yet."), nil
	}
	location, code := r.agy.locateContext(sessionID)
	if code != "" {
		return unavailableCode(code, "Antigravity conversation history is unavailable."), nil
	}
	entries, hasMore, corrupt, _, code := r.agy.readFromContext(context.Background(), location.Path, before, limit)
	if code != "" {
		return unavailableCode(code, "Antigravity conversation history is unavailable."), nil
	}
	normalizeEntriesForResponse(entries)
	return Page{
		Available:     true,
		Entries:       append([]Entry(nil), entries...),
		HasMore:       hasMore,
		Total:         UnknownTotal,
		SourceCorrupt: corrupt,
	}, nil
}
