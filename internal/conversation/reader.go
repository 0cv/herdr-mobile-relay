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
	"unicode/utf8"

	panehistory "github.com/0cv/herdr-mobile-relay/internal/history"
)

const (
	maxConversationBytes = 16 * 1024 * 1024
	maxEntryBytes        = 128 * 1024
	defaultPageSize      = 80
	maxPageSize          = 200
)

var canonicalSessionID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type Entry struct {
	ID        string `json:"id"`
	Timestamp string `json:"timestamp,omitempty"`
	Role      string `json:"role"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated,omitempty"`
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
	claudeRoots []string
	qoderRoots  []string
	codexRoots  []string
	piRoots     []string
	ompRoots    []string
}

func NewReader(home string) *Reader {
	claudeHome := firstConfiguredRoot("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	codexHome := firstConfiguredRoot("CODEX_HOME", filepath.Join(home, ".codex"))
	piHome := firstConfiguredRoot("PI_CODING_AGENT_DIR", filepath.Join(home, ".pi", "agent"))
	return &Reader{
		claudeRoots: []string{filepath.Join(claudeHome, "projects")},
		qoderRoots:  []string{filepath.Join(home, ".qoder", "projects")},
		codexRoots:  []string{filepath.Join(codexHome, "sessions")},
		piRoots:     []string{filepath.Join(piHome, "sessions")},
		ompRoots:    []string{filepath.Join(home, ".omp", "agent", "sessions")},
	}
}

func firstConfiguredRoot(environment, fallback string) string {
	if configured := strings.TrimSpace(os.Getenv(environment)); configured != "" {
		return configured
	}
	return fallback
}

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

func (r *Reader) Read(agent, sessionID, before string, limit int) (Page, error) {
	if !Supported(agent) {
		return unavailable("Conversation history is not available for this agent."), nil
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return unavailable("This agent has not reported a conversation session yet."), nil
	}
	path := r.resolve(agent, sessionID)
	if path == "" {
		return unavailable("No conversation log is available for this session."), nil
	}
	text, clipped, err := loadTail(path, maxConversationBytes)
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

func (r *Reader) resolve(agent, sessionID string) string {
	switch normalizedAgent(agent) {
	case "claude", "claudecode":
		if !safeSessionID(sessionID) {
			return ""
		}
		return findProjectSession(r.claudeRoots, sessionID+".jsonl")
	case "qoder", "qodercli":
		if !safeSessionID(sessionID) {
			return ""
		}
		return findProjectSession(r.qoderRoots, sessionID+".jsonl")
	case "codex", "openaicodex":
		if !canonicalSessionID.MatchString(sessionID) {
			return ""
		}
		return findCodexSession(r.codexRoots, sessionID)
	case "pi", "picodingagent":
		return resolvePathOrSession(r.piRoots, sessionID, "_")
	case "omp", "ohmypi":
		return resolvePathOrSession(r.ompRoots, sessionID, "_")
	default:
		return ""
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

func findProjectSession(roots []string, filename string) string {
	for _, root := range roots {
		directories, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, directory := range directories {
			if !directory.IsDir() {
				continue
			}
			candidate := filepath.Join(root, directory.Name(), filename)
			if path := containedRegularFile(candidate, root); path != "" {
				return path
			}
		}
	}
	return ""
}

func findCodexSession(roots []string, sessionID string) string {
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
						if file.IsDir() || !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, suffix) {
							continue
						}
						if path := containedRegularFile(filepath.Join(dayPath, file.Name()), root); path != "" {
							return path
						}
					}
				}
			}
		}
	}
	return ""
}

func descendingDirectories(path string) []string {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil
	}
	directories := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			directories = append(directories, entry.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(directories)))
	return directories
}

func resolvePathOrSession(roots []string, sessionID, separator string) string {
	if filepath.IsAbs(sessionID) && strings.HasSuffix(strings.ToLower(sessionID), ".jsonl") {
		for _, root := range roots {
			if path := containedRegularFile(sessionID, root); path != "" {
				return path
			}
		}
		return ""
	}
	if !canonicalSessionID.MatchString(sessionID) {
		return ""
	}
	suffix := separator + strings.ToLower(sessionID) + ".jsonl"
	for _, root := range roots {
		directories, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, directory := range directories {
			if !directory.IsDir() {
				continue
			}
			files, err := os.ReadDir(filepath.Join(root, directory.Name()))
			if err != nil {
				continue
			}
			for _, file := range files {
				if file.IsDir() || !strings.HasSuffix(strings.ToLower(file.Name()), suffix) {
					continue
				}
				if path := containedRegularFile(filepath.Join(root, directory.Name(), file.Name()), root); path != "" {
					return path
				}
			}
		}
	}
	return ""
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

func parseTranscript(agent, text string) []Entry {
	normalized := normalizedAgent(agent)
	entries := make([]Entry, 0)
	seenIDs := make(map[string]int)
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) != nil {
			continue
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
		if role == "" || body == "" {
			continue
		}
		body, truncated := clampText(body, maxEntryBytes)
		id := stableRowID(line, seenIDs)
		entries = append(entries, Entry{ID: id, Timestamp: timestamp, Role: role, Text: body, Truncated: truncated})
	}
	return entries
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
		return "", ""
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
	blocks, ok := value.([]any)
	if !ok {
		return ""
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
	return strings.Join(texts, "\n")
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
