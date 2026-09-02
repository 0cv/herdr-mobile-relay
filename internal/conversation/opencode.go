package conversation

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
)

const (
	openCodeQueryTimeout = 3 * time.Second
	maxOpenCodeOutput    = 8 * 1024 * 1024
	maxOpenCodeCache     = 128
)

var errOpenCodeOutputLimit = errors.New("opencode query output limit exceeded")

type openCodeReader struct {
	home   string
	binary string
	mu     sync.Mutex
	cache  map[string]openCodeCacheEntry
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
}

type boundedBuffer struct {
	bytes.Buffer
	remaining int
	overflow  bool
}

func (b *boundedBuffer) Write(data []byte) (int, error) {
	if len(data) > b.remaining {
		if b.remaining > 0 {
			_, _ = b.Buffer.Write(data[:b.remaining])
			b.remaining = 0
		}
		b.overflow = true
		return 0, errOpenCodeOutputLimit
	}
	b.remaining -= len(data)
	return b.Buffer.Write(data)
}

func newOpenCodeReader(home string) *openCodeReader {
	return &openCodeReader{home: home, binary: "sqlite3", cache: make(map[string]openCodeCacheEntry)}
}

func (r *openCodeReader) databases() ([]string, string) {
	if _, err := exec.LookPath(r.binary); err != nil {
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
		rows, hasMore, queryCode := r.query(database, sessionID, before, limit)
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
		entries, corrupt := parseOpenCodeRows(rows)
		if corrupt && len(entries) == 0 {
			return nil, false, true, openCodeRow{}, "source_corrupt"
		}
		return entries, hasMore, corrupt, rows[0], ""
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
	stamp, stampOK := openCodeStamp(database)
	cacheKey := fmt.Sprintf("%s\x00%s\x00%s\x00%d", database, sessionID, before, limit)
	if stampOK {
		if rows, hasMore, ok := r.cachedQuery(cacheKey, stamp); ok {
			return rows, hasMore, ""
		}
	}
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
	ctx, cancel := context.WithTimeout(context.Background(), openCodeQueryTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, r.binary, "-readonly", "-batch", "-json", database, query)
	stdout := &boundedBuffer{remaining: maxOpenCodeOutput}
	var stderr boundedBuffer
	stderr.remaining = 4096
	command.Stdout = stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if stdout.overflow || errors.Is(err, errOpenCodeOutputLimit) {
			return nil, false, "output_limit"
		}
		return nil, false, "query_failed"
	}
	var rows []openCodeRow
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		return nil, false, "source_corrupt"
	}
	for left, right := 0, len(rows)-1; left < right; left, right = left+1, right-1 {
		rows[left], rows[right] = rows[right], rows[left]
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
	if current, ok := openCodeStamp(database); stampOK && ok && current == stamp {
		r.storeQuery(cacheKey, stamp, rows, hasMore)
	}
	return rows, hasMore, ""
}

func (r *Reader) readOpenCode(sessionID, before string, limit int) (Page, error) {
	return r.readOpenCodeFor("", sessionID, before, limit)
}

func (r *Reader) readOpenCodeFor(cwd, sessionID, before string, limit int) (Page, error) {
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
	if cwd != "" && !sameOpenCodeDirectory(cwd, metadata.Directory) {
		return unavailableCode("invalid_session", "This conversation belongs to a different workspace."), nil
	}
	return Page{
		Available: true, Entries: append([]Entry(nil), entries...),
		HasMore: hasMore, Total: metadata.Total, SourceCorrupt: corrupt,
	}, nil
}

func sameOpenCodeDirectory(cwd, directory string) bool {
	realCwd, cwdErr := filepath.EvalSymlinks(filepath.Clean(cwd))
	realDirectory, directoryErr := filepath.EvalSymlinks(filepath.Clean(directory))
	return cwdErr == nil && directoryErr == nil && realCwd == realDirectory
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
	entries := make([]Entry, 0)
	byMessage := make(map[string]int)
	corrupt := false
	for _, row := range rows {
		if row.MessageID == "" {
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
				Status string          `json:"status"`
				Input  json.RawMessage `json:"input"`
				Output string          `json:"output"`
				Error  string          `json:"error"`
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
			activity.Error = part.State.Status == "error" || strings.TrimSpace(part.State.Error) != ""
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
