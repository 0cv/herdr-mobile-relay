package conversation

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// grokUpdatesName is the append-only session update stream Grok CLI writes
	// under <root>/<encoded cwd>/<session id>/ and replays on resume. Unlike
	// chat_history.jsonl it keeps turns from before a /compact.
	grokUpdatesName     = "updates.jsonl"
	grokSummaryName     = "summary.json"
	maxGrokRecordBytes  = maxConversationBytes
	maxGrokSummaryBytes = 256 * 1024
	grokCancelInterval  = 1024
)

// grokProjectDirName mirrors Rust urlencoding::encode, which Grok uses for
// project directory names: every byte except A-Z a-z 0-9 - _ . ~ becomes an
// uppercase %XX escape.
func grokProjectDirName(cwd string) string {
	const hexDigits = "0123456789ABCDEF"
	var builder strings.Builder
	builder.Grow(len(cwd) * 3)
	for index := 0; index < len(cwd); index++ {
		character := cwd[index]
		switch {
		case character >= 'A' && character <= 'Z', character >= 'a' && character <= 'z', character >= '0' && character <= '9',
			character == '-', character == '_', character == '.', character == '~':
			builder.WriteByte(character)
		default:
			builder.WriteByte('%')
			builder.WriteByte(hexDigits[character>>4])
			builder.WriteByte(hexDigits[character&0x0f])
		}
	}
	return builder.String()
}

// grokProjectDirNames reports the encoded cwd and, when it differs, the
// encoded real path: Grok stores the resolved directory, so /tmp/x lives
// under %2Fprivate%2Ftmp%2Fx on macOS.
func grokProjectDirNames(cwd string) []string {
	names := []string{grokProjectDirName(cwd)}
	if real, err := filepath.EvalSymlinks(cwd); err == nil && real != cwd {
		names = append(names, grokProjectDirName(real))
	}
	return names
}

// findGrokSession tries the project directory for each known cwd first, then
// scans every project directory for the session id.
func findGrokSession(roots []string, project ProjectContext, sessionID string) Location {
	for _, root := range roots {
		seen := make(map[string]bool)
		try := func(projectDir string) Location {
			projectDir = filepath.Clean(projectDir)
			if seen[projectDir] {
				return Location{}
			}
			seen[projectDir] = true
			sessionDir := filepath.Join(projectDir, sessionID)
			path := containedRegularFile(filepath.Join(sessionDir, grokUpdatesName), root)
			if path == "" {
				return Location{}
			}
			return Location{Path: path, Root: root, Title: grokSessionTitle(filepath.Join(sessionDir, grokSummaryName), root)}
		}
		for _, cwd := range projectDirectoriesForContext(project) {
			if strings.TrimSpace(cwd) == "" {
				continue
			}
			for _, name := range grokProjectDirNames(cwd) {
				if location := try(filepath.Join(root, name)); location.Path != "" {
					return location
				}
			}
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			projectDir := filepath.Join(root, entry.Name())
			if !isDir(projectDir) {
				continue
			}
			if location := try(projectDir); location.Path != "" {
				return location
			}
		}
	}
	return Location{}
}

// grokSessionTitle reads the session title from summary.json beside the
// update stream. session_summary is preferred; generated_title is the model
// title Grok writes once the session has a first turn.
func grokSessionTitle(path, root string) string {
	contained := containedRegularFile(path, root)
	if contained == "" {
		return ""
	}
	file, err := os.Open(contained)
	if err != nil {
		return ""
	}
	defer file.Close()
	var summary struct {
		SessionSummary string `json:"session_summary"`
		GeneratedTitle string `json:"generated_title"`
	}
	if json.NewDecoder(io.LimitReader(file, maxGrokSummaryBytes)).Decode(&summary) != nil {
		return ""
	}
	title := sanitizeText(summary.SessionSummary)
	if title == "" {
		title = sanitizeText(summary.GeneratedTitle)
	}
	title, _ = clampText(title, maxToolNameBytes)
	return title
}

type grokRecord struct {
	Timestamp int64 `json:"timestamp"`
	Params    struct {
		Update grokUpdate `json:"update"`
		Meta   struct {
			AgentTimestampMs int64 `json:"agentTimestampMs"`
		} `json:"_meta"`
	} `json:"params"`
}

type grokUpdate struct {
	SessionUpdate string          `json:"sessionUpdate"`
	Content       json.RawMessage `json:"content"`
	ToolCallID    string          `json:"toolCallId"`
	Title         string          `json:"title"`
	RawInput      json.RawMessage `json:"rawInput"`
	RawOutput     json.RawMessage `json:"rawOutput"`
	Status        string          `json:"status"`
	Meta          struct {
		PromptIndex        *int64 `json:"promptIndex"`
		HideFromScrollback bool   `json:"hideFromScrollback"`
	} `json:"_meta"`
}

type grokContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type grokToolContent struct {
	Type    string           `json:"type"`
	Content grokContentBlock `json:"content"`
}

// grokRelevantKinds are the update kinds that produce visible rows. Lines that
// do not mention any of them (hook_execution, thoughts, plans, compaction
// markers, ...) are skipped without decoding.
var grokRelevantKinds = [][]byte{
	[]byte(`"user_message_chunk"`), []byte(`"agent_message_chunk"`),
	[]byte(`"tool_call"`), []byte(`"tool_call_update"`),
}

type grokPending struct {
	entry       Entry
	index       int
	text        strings.Builder
	clipped     bool
	prompt      *int64
	acceptsText bool
}

type grokProjector struct {
	entries   []*grokPending
	tools     map[string]toolLocation
	user      *grokPending
	assistant *grokPending
	corrupt   bool
}

func newGrokProjector() *grokProjector {
	return &grokProjector{tools: make(map[string]toolLocation)}
}

func (p *grokProjector) start(role string, offset int64, timestamp string) *grokPending {
	pending := &grokPending{
		entry: Entry{ID: strconv.FormatInt(offset, 10), Role: role, Timestamp: timestamp},
		index: len(p.entries),
	}
	p.entries = append(p.entries, pending)
	return pending
}

func (p *grokProjector) apply(line []byte, offset int64) {
	var record grokRecord
	if json.Unmarshal(line, &record) != nil {
		p.corrupt = true
		return
	}
	update := record.Params.Update
	timestamp := grokTimestamp(record.Params.Meta.AgentTimestampMs, record.Timestamp)
	switch update.SessionUpdate {
	case "user_message_chunk":
		p.applyUser(update, offset, timestamp)
	case "agent_message_chunk":
		p.applyAgent(update, offset, timestamp)
	case "tool_call":
		p.applyToolCall(update, offset, timestamp)
	case "tool_call_update":
		p.applyToolUpdate(update)
	}
}

func (p *grokProjector) applyUser(update grokUpdate, offset int64, timestamp string) {
	p.assistant = nil
	if update.Meta.HideFromScrollback {
		// Injected <system-reminder> notices (background tasks, subagents).
		p.user = nil
		return
	}
	var block grokContentBlock
	if json.Unmarshal(update.Content, &block) != nil {
		p.corrupt = true
		return
	}
	if block.Type != "text" {
		return
	}
	prompt := update.Meta.PromptIndex
	current := p.user
	if current == nil || current.prompt == nil || prompt == nil || *current.prompt != *prompt {
		current = p.start("user", offset, timestamp)
		current.prompt = prompt
		p.user = current
	} else if current.text.Len() > 0 {
		appendGrokText(current, "\n")
	}
	appendGrokText(current, block.Text)
}

func (p *grokProjector) applyAgent(update grokUpdate, offset int64, timestamp string) {
	p.user = nil
	var block grokContentBlock
	if json.Unmarshal(update.Content, &block) != nil {
		p.corrupt = true
		return
	}
	if block.Type != "text" || block.Text == "" {
		return
	}
	if p.assistant == nil || !p.assistant.acceptsText {
		p.assistant = p.start("assistant", offset, timestamp)
		p.assistant.acceptsText = true
	}
	appendGrokText(p.assistant, block.Text)
}

func (p *grokProjector) applyToolCall(update grokUpdate, offset int64, timestamp string) {
	p.user = nil
	id := strings.TrimSpace(update.ToolCallID)
	if id == "" {
		p.corrupt = true
		return
	}
	if p.assistant == nil {
		p.assistant = p.start("assistant", offset, timestamp)
	}
	// Text after a tool call belongs to a later message.
	p.assistant.acceptsText = false
	tool := newToolActivity(id, update.Title, grokRawJSON(update.RawInput))
	p.assistant.entry.Tools = append(p.assistant.entry.Tools, tool)
	p.tools[id] = toolLocation{entry: p.assistant.index, tool: len(p.assistant.entry.Tools) - 1}
}

// applyToolUpdate associates results by toolCallId, so parallel results that
// arrive out of order still reach their own call. The last status and the
// last non-empty output win.
func (p *grokProjector) applyToolUpdate(update grokUpdate) {
	location, ok := p.tools[strings.TrimSpace(update.ToolCallID)]
	if !ok {
		return
	}
	tool := &p.entries[location.entry].entry.Tools[location.tool]
	if tool.Input == "" {
		if input := grokRawJSON(update.RawInput); input != "" {
			var truncated bool
			tool.Input, truncated = clampText(sanitizeText(input), maxEntryBytes/2)
			tool.Truncated = tool.Truncated || truncated
		}
	}
	if update.Status != "" {
		tool.Error = update.Status == "failed"
	}
	output, corrupt := grokToolOutput(update)
	if corrupt {
		p.corrupt = true
	}
	if output == "" {
		return
	}
	var truncated bool
	tool.Output, truncated = clampText(output, maxEntryBytes)
	tool.Truncated = tool.Truncated || truncated
}

// grokToolOutput prefers the text blocks Grok renders; when there are none
// (edits, backend searches) it falls back to the structured rawOutput. Image
// blocks are omitted.
func grokToolOutput(update grokUpdate) (string, bool) {
	corrupt := false
	texts := make([]string, 0, 1)
	if len(update.Content) > 0 && string(update.Content) != "null" {
		var blocks []grokToolContent
		if json.Unmarshal(update.Content, &blocks) != nil {
			corrupt = true
		}
		for _, block := range blocks {
			if block.Type == "content" && block.Content.Type == "text" && block.Content.Text != "" {
				texts = append(texts, block.Content.Text)
			}
		}
	}
	output := sanitizeText(strings.Join(texts, "\n"))
	if output == "" {
		output = sanitizeText(grokRawJSON(update.RawOutput))
	}
	return output, corrupt
}

func grokRawJSON(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return ""
	}
	return string(trimmed)
}

func appendGrokText(pending *grokPending, text string) {
	if pending.clipped {
		return
	}
	// Keep headroom for sanitising; the final clamp enforces maxEntryBytes.
	room := 2*maxEntryBytes - pending.text.Len()
	if len(text) > room {
		text, _ = clampText(text, room)
		pending.clipped = true
	}
	pending.text.WriteString(text)
}

func (p *grokProjector) finish() []Entry {
	entries := make([]Entry, 0, len(p.entries))
	for _, pending := range p.entries {
		entry := pending.entry
		text := pending.text.String()
		if entry.Role == "user" {
			if query := innerTag(text, "user_query"); query != "" {
				text = query
			}
		}
		text = sanitizeText(text)
		var truncated bool
		entry.Text, truncated = clampText(text, maxEntryBytes)
		entry.Truncated = entry.Truncated || truncated || pending.clipped
		if entry.Text == "" && len(entry.Tools) == 0 {
			continue
		}
		normalizeEntryTools(&entry)
		entries = append(entries, entry)
	}
	return entries
}

func grokTimestamp(agentMillis, seconds int64) string {
	switch {
	case agentMillis > 0:
		return time.UnixMilli(agentMillis).UTC().Format(time.RFC3339Nano)
	case seconds > 0:
		return time.Unix(seconds, 0).UTC().Format(time.RFC3339Nano)
	default:
		return ""
	}
}

// parseGrokUpdates streams updates.jsonl line by line. Entry IDs are the byte
// offset of each entry's first record, which is stable while the file is only
// appended; replacement is caught by the source identity revision.
func parseGrokUpdates(ctx context.Context, path string) ([]Entry, bool, error) {
	file, err := openConversationSource(path)
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, errors.New("conversation source is not a regular file")
	}
	return projectGrokUpdates(ctx, file)
}

func projectGrokUpdates(ctx context.Context, source io.Reader) ([]Entry, bool, error) {
	reader := bufio.NewReaderSize(source, 256*1024)
	projector := newGrokProjector()
	var line []byte
	var offset int64
	for count := 0; ; count++ {
		if count%grokCancelInterval == 0 {
			if err := ctx.Err(); err != nil {
				return nil, false, err
			}
		}
		start := offset
		var consumed int64
		var oversized, complete bool
		var err error
		line, consumed, oversized, complete, err = readGrokLine(reader, line)
		offset += consumed
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, false, err
		}
		switch {
		case oversized:
			projector.corrupt = true
		case !grokRelevantLine(line):
		case !complete && !json.Valid(line):
			// A trailing record still being written is not corruption.
		default:
			projector.apply(line, start)
		}
		if errors.Is(err, io.EOF) {
			return projector.finish(), projector.corrupt, nil
		}
	}
}

// readGrokLine returns the next line without its newline, how many bytes it
// consumed, whether it exceeded maxGrokRecordBytes (its content is then
// discarded), and whether it ended with a newline.
func readGrokLine(reader *bufio.Reader, buffer []byte) ([]byte, int64, bool, bool, error) {
	buffer = buffer[:0]
	var consumed int64
	oversized := false
	for {
		chunk, err := reader.ReadSlice('\n')
		consumed += int64(len(chunk))
		if !oversized {
			if len(buffer)+len(chunk) > maxGrokRecordBytes+1 {
				oversized = true
				buffer = buffer[:0]
			} else {
				buffer = append(buffer, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		complete := err == nil
		if complete && !oversized {
			buffer = buffer[:len(buffer)-1]
		}
		return bytes.TrimSpace(buffer), consumed, oversized, complete, err
	}
}

func grokRelevantLine(line []byte) bool {
	if len(line) == 0 {
		return false
	}
	for _, kind := range grokRelevantKinds {
		if bytes.Contains(line, kind) {
			return true
		}
	}
	return false
}

// grokPage returns the limit entries before the entry whose ID is before (or
// the newest entries), with hasMore and the visible total.
func grokPage(entries []Entry, before string, limit int) ([]Entry, bool, int, bool) {
	end := len(entries)
	if before != "" {
		end = -1
		for index := range entries {
			if entries[index].ID == before {
				end = index
				break
			}
		}
		if end < 0 {
			return nil, false, 0, false
		}
	}
	start := end - limit
	if start < 0 {
		start = 0
	}
	return append([]Entry(nil), entries[start:end]...), start > 0, len(entries), true
}

func validGrokCursor(before string) bool {
	if before == "" {
		return true
	}
	value, err := strconv.ParseInt(before, 10, 64)
	return err == nil && value >= 0
}

func (r *Reader) readGrokFor(project ProjectContext, sessionID, before string, limit int) (Page, error) {
	sessionID = strings.TrimSpace(sessionID)
	if !safeSessionID(sessionID) {
		return unavailableCode("invalid_session", "This agent has not reported a conversation session yet."), nil
	}
	if !validGrokCursor(before) {
		return unavailableCode("invalid_cursor", "This conversation page cursor is invalid."), nil
	}
	if limit < 1 {
		limit = defaultPageSize
	}
	if limit > maxPageSize {
		limit = maxPageSize
	}
	location := r.LocateWithProject("grok", project, sessionID)
	if location.Path == "" {
		return unavailableCode("invalid_session", "No conversation log is available for this session."), nil
	}
	entries, corrupt, err := parseGrokUpdates(context.Background(), location.Path)
	if err != nil {
		return Page{}, fmt.Errorf("read conversation log: %w", err)
	}
	page, hasMore, total, ok := grokPage(entries, before, limit)
	if !ok {
		return unavailableCode("invalid_cursor", "This conversation page cursor is invalid."), nil
	}
	normalizeEntriesForResponse(page)
	return Page{Available: true, Entries: page, HasMore: hasMore, Total: total, SourceCorrupt: corrupt}, nil
}
