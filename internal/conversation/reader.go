package conversation

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
	panehistory "github.com/0cv/herdr-mobile-relay/internal/history"
)

const (
	maxConversationBytes    = 16 * 1024 * 1024
	maxEntryBytes           = 128 * 1024
	defaultPageSize         = 80
	maxPageSize             = 200
	locationCacheTTL        = 60 * time.Second
	locationMissTTL         = 5 * time.Second
	maxLocationCacheEntries = 2048
)

var canonicalSessionID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type ToolActivity struct {
	ID        string `json:"id,omitempty"`
	Name      string `json:"name"`
	Input     string `json:"input,omitempty"`
	Output    string `json:"output,omitempty"`
	Error     bool   `json:"error,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

type Entry struct {
	ID        string         `json:"id"`
	Timestamp string         `json:"timestamp,omitempty"`
	Role      string         `json:"role"`
	Text      string         `json:"text,omitempty"`
	Tools     []ToolActivity `json:"tools,omitempty"`
	Truncated bool           `json:"truncated,omitempty"`
}

type Page struct {
	Available     bool    `json:"available"`
	Reason        string  `json:"reason,omitempty"`
	Entries       []Entry `json:"entries"`
	HasMore       bool    `json:"has_more"`
	Total         int     `json:"total"`
	FileTruncated bool    `json:"file_truncated,omitempty"`
}
type Reader struct {
	home      string
	mu        sync.Mutex
	locations map[string]locationCacheEntry
	locating  map[string]chan struct{}
}

type locationCacheEntry struct {
	location Location
	expires  time.Time
}

// Location identifies the exact transcript selected for a pane and the
// configured root that contains it. Session-title resolution consumes the same
// location so it cannot read a different copy of the session.
type Location struct {
	Path string
	Root string
}

// NewReader keeps a bounded tuple-to-location cache. Sharing one Reader between
func NewReader(home string) *Reader {
	return &Reader{
		home: home, locations: make(map[string]locationCacheEntry),
		locating: make(map[string]chan struct{}),
	}
}

func (r *Reader) claudeRoots() []string { return agentroots.Claude(r.home) }

func (r *Reader) qoderRoots() []string { return agentroots.Qoder(r.home) }

func (r *Reader) codexRoots() []string { return agentroots.Codex(r.home) }

func (r *Reader) piRoots() []string { return agentroots.Pi(r.home) }

func (r *Reader) ompRoots() []string { return agentroots.OMP(r.home) }

func Supported(agent string) bool {
	switch normalizedAgent(agent) {
	case "claude", "claudecode", "qoder", "qodercli", "codex", "openaicodex",
		"pi", "picodingagent", "omp", "ohmypi":
		return true
	default:
		return false
	}
}

func normalizedAgent(agent string) string {
	value := strings.ToLower(strings.TrimSpace(agent))
	value = strings.NewReplacer("-", "", "_", "", " ", "").Replace(value)
	return value
}

// Read resolves a conversation without project context. It is used for
// historical activity where the pane cwd is no longer available.
func (r *Reader) Read(agent, sessionID, before string, limit int) (Page, error) {
	return r.read(agent, "", sessionID, before, limit)
}

// ReadFor resolves a pane conversation using the same cwd-aware locator as the
// session-title resolver.
func (r *Reader) ReadFor(agent, cwd, sessionID, before string, limit int) (Page, error) {
	return r.read(agent, cwd, sessionID, before, limit)
}

func (r *Reader) read(agent, cwd, sessionID, before string, limit int) (Page, error) {
	if !Supported(agent) {
		return unavailable("Conversation history is not available for this agent."), nil
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return unavailable("This agent has not reported a conversation session yet."), nil
	}
	location := r.Locate(agent, cwd, sessionID)
	if location.Path == "" {
		return unavailable("No conversation log is available for this session."), nil
	}
	text, clipped, err := loadTail(location.Path, maxConversationBytes)
	if err != nil {
		return Page{}, fmt.Errorf("read conversation log: %w", err)
	}
	entries := parseTranscript(agent, text)
	if limit < 1 {
		limit = defaultPageSize
	}
	if limit > maxPageSize {
		limit = maxPageSize
	}
	end := len(entries)
	if before != "" {
		for index := range entries {
			if entries[index].ID == before {
				end = index
				break
			}
		}
	}
	start := end - limit
	if start < 0 {
		start = 0
	}
	pageEntries := append([]Entry(nil), entries[start:end]...)
	return Page{
		Available:     true,
		Entries:       pageEntries,
		HasMore:       start > 0,
		Total:         len(entries),
		FileTruncated: clipped,
	}, nil
}

func unavailable(reason string) Page {
	return Page{Available: false, Reason: reason, Entries: []Entry{}}
}

// Locate returns the exact contained transcript selected for agent, cwd and
// sessionID. Root order is authoritative; within Claude/Qoder roots cwd selects
// the project directory when it is known. Cached tuple locations are returned
// before any filesystem walk, keeping title and history on the same copy.
func (r *Reader) Locate(agent, cwd, sessionID string) Location {
	sessionID = strings.TrimSpace(sessionID)
	key := normalizedAgent(agent) + "\x00" + cwd + "\x00" + sessionID
	for {
		now := time.Now()
		r.mu.Lock()
		if cached, ok := r.locations[key]; ok && now.Before(cached.expires) {
			r.mu.Unlock()
			return cached.location
		}
		if ready, ok := r.locating[key]; ok {
			r.mu.Unlock()
			<-ready
			continue
		}
		r.locating[key] = make(chan struct{})
		r.mu.Unlock()
		break
	}

	location := r.locate(agent, cwd, sessionID)
	ttl := locationCacheTTL
	if location.Path == "" {
		ttl = locationMissTTL
	}
	now := time.Now()
	r.mu.Lock()
	if len(r.locations) >= maxLocationCacheEntries {
		for cachedKey, cached := range r.locations {
			if !now.Before(cached.expires) {
				delete(r.locations, cachedKey)
			}
		}
		if len(r.locations) >= maxLocationCacheEntries {
			clear(r.locations)
		}
	}
	r.locations[key] = locationCacheEntry{location: location, expires: now.Add(ttl)}
	ready := r.locating[key]
	delete(r.locating, key)
	close(ready)
	r.mu.Unlock()
	return location
}

func (r *Reader) locate(agent, cwd, sessionID string) Location {
	switch normalizedAgent(agent) {
	case "claude", "claudecode":
		if !safeSessionID(sessionID) {
			return Location{}
		}
		return findProjectSession(r.claudeRoots(), cwd, sessionID+".jsonl")
	case "qoder", "qodercli":
		if !safeSessionID(sessionID) {
			return Location{}
		}
		return findProjectSession(r.qoderRoots(), cwd, sessionID+".jsonl")
	case "codex", "openaicodex":
		if !canonicalSessionID.MatchString(sessionID) {
			return Location{}
		}
		return findCodexSession(r.codexRoots(), sessionID)
	case "pi", "picodingagent":
		return resolvePathOrSession(r.piRoots(), sessionID, "_")
	case "omp", "ohmypi":
		return resolvePathOrSession(r.ompRoots(), sessionID, "_")
	default:
		return Location{}
	}
}

func safeSessionID(value string) bool {
	if value == "" || len(value) > 128 || value == "." || value == ".." {
		return false
	}
	for _, character := range value {
		switch {
		case character >= 'a' && character <= 'z', character >= 'A' && character <= 'Z', character >= '0' && character <= '9':
		case character == '-', character == '_', character == '.':
		default:
			return false
		}
	}
	return true
}

// isDir reports whether path is a directory, following symlinks. DirEntry's
// IsDir reports the entry's own type bits, which are ModeSymlink for a
// symlink, so it is false for a symlink to a directory - the same hazard
// agentroots.profileAgentDirs documents. Stat follows the link so a
// symlinked project or profile directory is still searched.
func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func findProjectSession(roots []string, cwd, filename string) Location {
	for _, root := range roots {
		for _, projectDir := range projectDirectories(root, cwd) {
			if path := containedRegularFile(filepath.Join(projectDir, filename), root); path != "" {
				return Location{Path: path, Root: root}
			}
		}
	}
	return Location{}
}

func projectDirectories(root, cwd string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	if strings.TrimSpace(cwd) == "" {
		directories := make([]string, 0, len(entries))
		for _, entry := range entries {
			path := filepath.Join(root, entry.Name())
			if isDir(path) {
				directories = append(directories, path)
			}
		}
		return directories
	}

	encoded := strings.ReplaceAll(strings.TrimPrefix(cwd, "/"), "/", "-")
	exact := map[string]bool{encoded: true, "-" + encoded: true}
	directories := make([]string, 0, 2)
	seen := make(map[string]bool, 2)
	for _, entry := range entries {
		if !exact[entry.Name()] {
			continue
		}
		path := filepath.Join(root, entry.Name())
		if isDir(path) {
			directories = append(directories, path)
			seen[path] = true
		}
	}
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		if seen[path] || !isDir(path) {
			continue
		}
		cwdFile := containedRegularFile(filepath.Join(path, "cwd"), root)
		if cwdFile == "" {
			continue
		}
		data, err := os.ReadFile(cwdFile)
		if err == nil && strings.TrimSpace(string(data)) == cwd {
			directories = append(directories, path)
		}
	}
	return directories
}

func findCodexSession(roots []string, sessionID string) Location {
	suffix := "-" + strings.ToLower(sessionID) + ".jsonl"
	for _, root := range roots {
		for _, year := range descendingDirectories(root) {
			yearPath := filepath.Join(root, year)
			for _, month := range descendingDirectories(yearPath) {
				monthPath := filepath.Join(yearPath, month)
				for _, day := range descendingDirectories(monthPath) {
					dayPath := filepath.Join(monthPath, day)
					files, err := os.ReadDir(dayPath)
					if err != nil {
						continue
					}
					for _, file := range files {
						name := strings.ToLower(file.Name())
						filePath := filepath.Join(dayPath, file.Name())
						if isDir(filePath) || !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, suffix) {
							continue
						}
						if path := containedRegularFile(filePath, root); path != "" {
							return Location{Path: path, Root: root}
						}
					}
				}
			}
		}
	}
	return Location{}
}

func descendingDirectories(path string) []string {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil
	}
	directories := make([]string, 0, len(entries))
	for _, entry := range entries {
		if isDir(filepath.Join(path, entry.Name())) {
			directories = append(directories, entry.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(directories)))
	return directories
}

func resolvePathOrSession(roots []string, sessionID, separator string) Location {
	if filepath.IsAbs(sessionID) && strings.HasSuffix(strings.ToLower(sessionID), ".jsonl") {
		for _, root := range roots {
			if path := containedRegularFile(sessionID, root); path != "" {
				return Location{Path: path, Root: root}
			}
		}
		return Location{}
	}
	if !canonicalSessionID.MatchString(sessionID) {
		return Location{}
	}
	suffix := separator + strings.ToLower(sessionID) + ".jsonl"
	for _, root := range roots {
		directories, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, directory := range directories {
			projectDir := filepath.Join(root, directory.Name())
			if !isDir(projectDir) {
				continue
			}
			files, err := os.ReadDir(projectDir)
			if err != nil {
				continue
			}
			for _, file := range files {
				filePath := filepath.Join(projectDir, file.Name())
				if isDir(filePath) || !strings.HasSuffix(strings.ToLower(file.Name()), suffix) {
					continue
				}
				if path := containedRegularFile(filePath, root); path != "" {
					return Location{Path: path, Root: root}
				}
			}
		}
	}
	return Location{}
}

func containedRegularFile(path, root string) string {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return ""
	}
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return ""
	}
	relative, err := filepath.Rel(realRoot, realPath)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return ""
	}
	info, err := os.Stat(realPath)
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	return realPath
}

func loadTail(path string, limit int64) (string, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", false, err
	}
	clipped := info.Size() > limit
	start := int64(0)
	if clipped {
		start = info.Size() - limit
	}
	if _, err := file.Seek(start, 0); err != nil {
		return "", false, err
	}
	data, err := io.ReadAll(io.LimitReader(file, limit))
	if err != nil {
		return "", false, err
	}
	if clipped {
		if newline := bytes.IndexByte(data, '\n'); newline >= 0 {
			data = data[newline+1:]
		} else {
			data = nil
		}
	}
	return string(data), clipped, nil
}

type toolResult struct {
	id     string
	output string
	failed bool
}

type toolLocation struct {
	entry int
	tool  int
}

func parseTranscript(agent, text string) []Entry {
	normalized := normalizedAgent(agent)
	entries := make([]Entry, 0)
	seenIDs := make(map[string]int)
	pendingTools := make(map[string]toolLocation)
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) != nil {
			continue
		}
		calls, results := parseToolActivity(normalized, record)
		for _, result := range results {
			location, ok := pendingTools[result.id]
			if !ok || location.entry >= len(entries) || location.tool >= len(entries[location.entry].Tools) {
				continue
			}
			output := sanitizeText(result.output)
			output, truncated := clampText(output, maxEntryBytes)
			tool := &entries[location.entry].Tools[location.tool]
			tool.Output = output
			tool.Error = result.failed
			tool.Truncated = tool.Truncated || truncated
			delete(pendingTools, result.id)
		}

		role, timestamp, body := "", stringValue(record["timestamp"]), ""
		switch normalized {
		case "claude", "claudecode", "qoder", "qodercli":
			role, body = parseClaudeRecord(record)
		case "codex", "openaicodex":
			role, body = parseCodexRecord(record)
		case "pi", "picodingagent", "omp", "ohmypi":
			role, body = parsePiRecord(record)
		}
		body = sanitizeText(body)
		if role == "" && len(calls) > 0 {
			role = "assistant"
		}
		if role == "" || (body == "" && len(calls) == 0) {
			continue
		}
		body, truncated := clampText(body, maxEntryBytes)
		id := stableRowID(line, seenIDs)
		entryIndex := len(entries)
		entries = append(entries, Entry{
			ID: id, Timestamp: timestamp, Role: role, Text: body, Tools: calls, Truncated: truncated,
		})
		for toolIndex := range calls {
			if calls[toolIndex].ID != "" {
				pendingTools[calls[toolIndex].ID] = toolLocation{entry: entryIndex, tool: toolIndex}
			}
		}
	}
	return entries
}

func parseToolActivity(agent string, record map[string]any) ([]ToolActivity, []toolResult) {
	switch agent {
	case "claude", "claudecode", "qoder", "qodercli":
		if record["isSidechain"] == true {
			return nil, nil
		}
		message, _ := record["message"].(map[string]any)
		blocks, _ := message["content"].([]any)
		return toolsFromBlocks(blocks)
	case "codex", "openaicodex":
		if stringValue(record["type"]) != "response_item" {
			return nil, nil
		}
		payload, _ := record["payload"].(map[string]any)
		switch normalizedBlockType(payload["type"]) {
		case "functioncall", "customtoolcall", "localshellcall":
			call := newToolActivity(
				firstString(payload, "call_id", "id"),
				firstString(payload, "name", "tool_name"),
				firstValue(payload, "arguments", "input"),
			)
			return []ToolActivity{call}, nil
		case "functioncalloutput", "customtoolcalloutput", "localshellcalloutput":
			return nil, []toolResult{{
				id:     firstString(payload, "call_id", "id"),
				output: textValue(firstValue(payload, "output", "content")),
				failed: payload["is_error"] == true,
			}}
		}
	case "pi", "picodingagent", "omp", "ohmypi":
		if stringValue(record["type"]) != "message" {
			return nil, nil
		}
		message, _ := record["message"].(map[string]any)
		if normalizedBlockType(message["role"]) == "toolresult" {
			return nil, []toolResult{{
				id:     firstString(message, "toolCallId", "tool_call_id", "id"),
				output: textValue(message["content"]),
				failed: message["isError"] == true || message["is_error"] == true,
			}}
		}
		blocks, _ := message["content"].([]any)
		return toolsFromBlocks(blocks)
	}
	return nil, nil
}

func toolsFromBlocks(blocks []any) ([]ToolActivity, []toolResult) {
	calls := make([]ToolActivity, 0)
	results := make([]toolResult, 0)
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch normalizedBlockType(block["type"]) {
		case "tooluse", "toolcall":
			calls = append(calls, newToolActivity(
				firstString(block, "id", "toolCallId", "tool_call_id"),
				firstString(block, "name", "toolName", "tool_name"),
				firstValue(block, "input", "arguments"),
			))
		case "toolresult":
			results = append(results, toolResult{
				id:     firstString(block, "tool_use_id", "toolCallId", "tool_call_id", "id"),
				output: textValue(block["content"]),
				failed: block["is_error"] == true || block["isError"] == true,
			})
		}
	}
	return calls, results
}

func newToolActivity(id, name string, input any) ToolActivity {
	if strings.TrimSpace(name) == "" {
		name = "Tool"
	}
	inputText := sanitizeText(textValue(input))
	inputText, truncated := clampText(inputText, maxEntryBytes/2)
	return ToolActivity{
		ID: strings.TrimSpace(id), Name: strings.TrimSpace(name), Input: inputText, Truncated: truncated,
	}
}

func normalizedBlockType(value any) string {
	return strings.NewReplacer("-", "", "_", "", " ", "").Replace(strings.ToLower(stringValue(value)))
}

func firstString(record map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := stringValue(record[key]); value != "" {
			return value
		}
	}
	return ""
}

func firstValue(record map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := record[key]; ok && value != nil {
			return value
		}
	}
	return nil
}

func textValue(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	if text := textBlocks(value); text != "" {
		return text
	}
	data, err := json.Marshal(value)
	if err != nil || string(data) == "null" {
		return ""
	}
	return string(data)
}

func parseClaudeRecord(record map[string]any) (string, string) {
	role := stringValue(record["type"])
	if (role != "user" && role != "assistant") || record["isSidechain"] == true {
		return "", ""
	}
	message, ok := record["message"].(map[string]any)
	if !ok {
		return "", ""
	}
	content := message["content"]
	if raw, ok := content.(string); ok {
		if role != "user" {
			return role, raw
		}
		return role, humanClaudeText(raw)
	}
	if role == "user" {
		// Filter per block, not on the joined string: humanClaudeText anchors its
		// envelope checks on the start of what it is given, so joining first
		// would let a <system-reminder> in any block after the first survive
		// into the phone's conversation view.
		blocks := textBlockList(content)
		kept := make([]string, 0, len(blocks))
		for _, block := range blocks {
			if text := humanClaudeText(block); strings.TrimSpace(text) != "" {
				kept = append(kept, text)
			}
		}
		if len(kept) == 0 {
			return "", ""
		}
		return role, strings.Join(kept, "\n")
	}
	return role, textBlocks(content)
}

func humanClaudeText(raw string) string {
	trimmed := strings.TrimSpace(raw)
	for _, envelope := range []string{"<system-reminder>", "<local-command-caveat>", "<task-notification>", "<local-command-stdout>"} {
		if strings.HasPrefix(trimmed, envelope) {
			return ""
		}
	}
	if strings.HasPrefix(trimmed, "<command-name>") {
		name := innerTag(trimmed, "command-name")
		arguments := innerTag(trimmed, "command-args")
		return strings.TrimSpace(name + " " + arguments)
	}
	return raw
}

func innerTag(text, name string) string {
	startToken, endToken := "<"+name+">", "</"+name+">"
	start := strings.Index(text, startToken)
	if start < 0 {
		return ""
	}
	start += len(startToken)
	end := strings.Index(text[start:], endToken)
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(text[start : start+end])
}

func parseCodexRecord(record map[string]any) (string, string) {
	if stringValue(record["type"]) != "response_item" {
		return "", ""
	}
	payload, ok := record["payload"].(map[string]any)
	if !ok || stringValue(payload["type"]) != "message" {
		return "", ""
	}
	role := stringValue(payload["role"])
	if role != "user" && role != "assistant" {
		return "", ""
	}
	text := textBlocks(payload["content"])
	if role == "user" && strings.HasPrefix(strings.TrimSpace(text), "<environment_context>") {
		return "", ""
	}
	return role, text
}

func parsePiRecord(record map[string]any) (string, string) {
	if stringValue(record["type"]) != "message" {
		return "", ""
	}
	message, ok := record["message"].(map[string]any)
	if !ok {
		return "", ""
	}
	role := stringValue(message["role"])
	if role != "user" && role != "assistant" {
		return "", ""
	}
	return role, textBlocks(message["content"])
}

func textBlocks(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return strings.Join(textBlockList(value), "\n")
}

// textBlockList reports the text carried by each text-like block separately.
// Callers that filter per block need the boundaries: joining first would let an
// envelope in a later block escape a prefix test anchored on the whole string.
func textBlockList(value any) []string {
	blocks, ok := value.([]any)
	if !ok {
		return nil
	}
	texts := make([]string, 0, len(blocks))
	for _, rawBlock := range blocks {
		block, ok := rawBlock.(map[string]any)
		if !ok {
			continue
		}
		typeName := stringValue(block["type"])
		if typeName != "text" && typeName != "input_text" && typeName != "output_text" {
			continue
		}
		if text := stringValue(block["text"]); text != "" {
			texts = append(texts, text)
		}
	}
	return texts
}

func sanitizeText(text string) string {
	text = panehistory.NormalizeLine(text)
	text = strings.ReplaceAll(text, "\x00", "")
	return strings.TrimSpace(text)
}

func clampText(text string, limit int) (string, bool) {
	if len(text) <= limit {
		return text, false
	}
	clipped := text[:limit]
	for !utf8.ValidString(clipped) {
		clipped = clipped[:len(clipped)-1]
	}
	return strings.TrimSpace(clipped), true
}

func stableRowID(line string, seen map[string]int) string {
	digest := sha256.Sum256([]byte(line))
	base := hex.EncodeToString(digest[:12])
	occurrence := seen[base]
	seen[base] = occurrence + 1
	if occurrence == 0 {
		return base
	}
	return fmt.Sprintf("%s-%d", base, occurrence)
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}
