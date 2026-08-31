package conversation

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
)

const (
	openCodeQueryTimeout = 3 * time.Second
	maxOpenCodeOutput    = 8 * 1024 * 1024
	maxOpenCodeRows      = 5000
)

var errOpenCodeOutputLimit = errors.New("opencode query output limit exceeded")

type openCodeReader struct {
	home   string
	binary string
}

type openCodeRow struct {
	SessionID   string          `json:"session_id"`
	Directory   string          `json:"directory"`
	Title       string          `json:"title"`
	Updated     int64           `json:"time_updated"`
	Agent       string          `json:"agent"`
	MessageID   string          `json:"message_id"`
	Created     int64           `json:"time_created"`
	MessageData json.RawMessage `json:"message_data"`
	PartID      string          `json:"part_id"`
	PartData    json.RawMessage `json:"part_data"`
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
	return &openCodeReader{home: home, binary: "sqlite3"}
}

func (r *openCodeReader) capability() (string, string) {
	if _, err := exec.LookPath(r.binary); err != nil {
		return "", "source_unavailable"
	}
	roots := agentroots.OpenCodeData(r.home)
	for index, candidate := range agentroots.OpenCodeDBs(r.home) {
		if index >= len(roots) {
			break
		}
		path := containedRegularFile(candidate, roots[index])
		if path != "" {
			return path, ""
		}
	}
	return "", "source_unavailable"
}

func (r *openCodeReader) read(sessionID string) ([]Entry, bool, openCodeRow, string) {
	if !validOpenCodeSessionID(sessionID) {
		return nil, false, openCodeRow{}, "invalid_session"
	}
	database, code := r.capability()
	if code != "" {
		return nil, false, openCodeRow{}, code
	}
	rows, clipped, code := r.query(database, sessionID)
	if code != "" {
		return nil, false, openCodeRow{}, code
	}
	if len(rows) == 0 || rows[0].SessionID != sessionID {
		return nil, false, openCodeRow{}, "invalid_session"
	}
	entries, corrupt := parseOpenCodeRows(rows)
	if corrupt && len(entries) == 0 {
		return nil, clipped, openCodeRow{}, "source_corrupt"
	}
	return entries, clipped || corrupt, rows[0], ""
}

func (r *openCodeReader) query(database, sessionID string) ([]openCodeRow, bool, string) {
	// The only substitution is a hex encoding of an already validated opaque
	// session ID. It remains data inside CAST(X'…' AS TEXT), never SQL syntax.
	sessionHex := hex.EncodeToString([]byte(sessionID))
	query := fmt.Sprintf(`SELECT s.id AS session_id,s.directory,s.title,s.time_updated,COALESCE(s.agent,'') AS agent,COALESCE(m.id,'') AS message_id,COALESCE(m.time_created,0) AS time_created,COALESCE(m.data,'null') AS message_data,COALESCE(p.id,'') AS part_id,COALESCE(p.data,'null') AS part_data FROM session AS s LEFT JOIN message AS m ON m.session_id=s.id LEFT JOIN part AS p ON p.message_id=m.id WHERE s.id=CAST(X'%s' AS TEXT) ORDER BY m.time_created DESC,m.id DESC,p.id DESC LIMIT %d;`, sessionHex, maxOpenCodeRows+1)
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
	clipped := len(rows) > maxOpenCodeRows
	if clipped {
		rows = rows[:maxOpenCodeRows]
	}
	for left, right := 0, len(rows)-1; left < right; left, right = left+1, right-1 {
		rows[left], rows[right] = rows[right], rows[left]
	}
	return rows, clipped, ""
}

func (r *Reader) readOpenCode(sessionID, before string, limit int) (Page, error) {
	return r.readOpenCodeFor("", sessionID, before, limit)
}

func (r *Reader) readOpenCodeFor(cwd, sessionID, before string, limit int) (Page, error) {
	sessionID = strings.TrimSpace(sessionID)
	entries, clipped, metadata, code := r.openCode.read(sessionID)
	if code != "" {
		return unavailableCode(code, "OpenCode conversation history is unavailable."), nil
	}
	if cwd != "" && !sameOpenCodeDirectory(cwd, metadata.Directory) {
		return unavailableCode("invalid_session", "This conversation belongs to a different workspace."), nil
	}
	if limit < 1 {
		limit = defaultPageSize
	}
	if limit > maxPageSize {
		limit = maxPageSize
	}
	end := len(entries)
	if before != "" {
		found := false
		for index := range entries {
			if entries[index].ID == before {
				end = index
				found = true
				break
			}
		}
		if !found {
			return unavailableCode("invalid_cursor", "The conversation cursor is invalid."), nil
		}
	}
	start := end - limit
	if start < 0 {
		start = 0
	}
	return Page{
		Available: true, Entries: append([]Entry(nil), entries[start:end]...),
		HasMore: start > 0, Total: len(entries), FileTruncated: clipped,
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
			if json.Unmarshal(row.MessageData, &message) != nil || (message.Role != "user" && message.Role != "assistant") {
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
		if json.Unmarshal(row.PartData, &part) != nil {
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
