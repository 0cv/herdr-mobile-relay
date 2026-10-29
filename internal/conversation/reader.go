package conversation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	"github.com/0cv/herdr-mobile-relay/internal/conversationlife"
	panehistory "github.com/0cv/herdr-mobile-relay/internal/history"
)

const (
	maxConversationBytes    = 16 * 1024 * 1024
	maxEntryBytes           = 128 * 1024
	maxToolCount            = 128
	maxToolIDBytes          = 256
	maxToolNameBytes        = 160
	maxToolInputBytes       = 1024 * 1024
	maxToolOutputBytes      = 1024 * 1024
	defaultPageSize         = 80
	maxPageSize             = 200
	locationCacheTTL        = 60 * time.Second
	locationMissTTL         = 5 * time.Second
	maxLocationCacheEntries = 2048
	maxOMOCacheEntries      = 8
)

var (
	canonicalSessionID           = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	claudeProjectNonAlphanumeric = regexp.MustCompile(`[^A-Za-z0-9]`)
)

type ToolActivity struct {
	ID            string `json:"id,omitempty"`
	Name          string `json:"name"`
	associationID string
	Input         string `json:"input,omitempty"`
	Output        string `json:"output,omitempty"`
	Error         bool   `json:"error,omitempty"`
	Truncated     bool   `json:"truncated,omitempty"`
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
	Available              bool          `json:"available"`
	ReasonCode             string        `json:"reason_code,omitempty"`
	Reason                 string        `json:"reason,omitempty"`
	Entries                []Entry       `json:"entries"`
	HasMore                bool          `json:"has_more"`
	Total                  int           `json:"total"`
	FileTruncated          bool          `json:"file_truncated,omitempty"`
	SourceCorrupt          bool          `json:"source_corrupt,omitempty"`
	ContinuationIncomplete bool          `json:"continuation_incomplete,omitempty"`
	ContinuationReason     string        `json:"continuation_reason,omitempty"`
	OMOPlan                *OMOTodoState `json:"omo_plan,omitempty"`
}
type locationKey struct {
	Agent         string
	CWD           string
	ForegroundCWD string
	SessionID     string
}

type Reader struct {
	home      string
	mu        sync.Mutex
	locations map[locationKey]locationCacheEntry
	locating  map[locationKey]chan struct{}
	installed *installedReaderBinding
	omoCache  map[string]omoCacheEntry
	openCode  *openCodeReader
	hermes    *hermesReader
}

type locationCacheEntry struct {
	location Location
	expires  time.Time
}

// Location identifies the exact transcript selected for a pane and the
// configured root that contains it. Session-title resolution consumes the same
// location so it cannot read a different copy of the session.
type Location struct {
	Path  string
	Root  string
	Title string
}

// NewReader keeps a bounded tuple-to-location cache. Sharing one Reader between
// the conversation and session packages makes their root selection identical.
func NewReader(home string) *Reader {
	return &Reader{
		home: home, locations: make(map[locationKey]locationCacheEntry),
		locating: make(map[locationKey]chan struct{}), omoCache: make(map[string]omoCacheEntry),
		openCode: newOpenCodeReader(home), hermes: newHermesReader(home),
	}
}

func (r *Reader) claudeRoots() []string { return agentroots.Claude(r.home) }

func (r *Reader) qoderRoots() []string { return agentroots.Qoder(r.home) }

func (r *Reader) codexRoots() []string { return agentroots.Codex(r.home) }

func (r *Reader) piRoots() []string { return agentroots.Pi(r.home) }

func (r *Reader) ompRoots() []string { return agentroots.OMP(r.home) }

func (r *Reader) cursorRoots() []string { return agentroots.Cursor(r.home) }

func Supported(agent string) bool {
	switch normalizedAgent(agent) {
	case "claude", "claudecode", "qoder", "qodercli", "codex", "openaicodex",
		"pi", "picodingagent", "omp", "ohmypi", "opencode", "omo", "ohmyopencode",
		"hermes", "hermesagent", "cursor", "cursoragent":
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
	return r.ReadWithProject(agent, ProjectContext{}, sessionID, before, limit)
}

// ReadFor resolves a pane conversation using the same cwd-aware locator as the
// session-title resolver.
func (r *Reader) ReadFor(agent, cwd, sessionID, before string, limit int) (Page, error) {
	return r.ReadWithProject(agent, ProjectContext{CWD: cwd}, sessionID, before, limit)
}

// read preserves the old package-local helper shape for tests and legacy
// callers; public callers should use Read or ReadFor.
func (r *Reader) read(agent, cwd, sessionID, before string, limit int) (Page, error) {
	return r.ReadWithProject(agent, ProjectContext{CWD: cwd}, sessionID, before, limit)
}

// ReadWithProject resolves a conversation with the pane and, for Claude Code,
// foreground directory hints. Once the anchor is located, Claude continuation
// descendants stay in that anchor's project and root.
func (r *Reader) ReadWithProject(agent string, project ProjectContext, sessionID, before string, limit int) (Page, error) {
	if r != nil && r.installed != nil {
		return r.ReadWithProjectContext(context.Background(), agent, project, sessionID, before, limit)
	}
	project = NormalizeProjectContext(agent, project)
	if isHermesAgent(agent) {
		return r.readHermesFor(agent, project.CWD, sessionID, before, limit)
	}
	if normalizedAgent(agent) == "opencode" {
		return r.readOpenCode(sessionID, before, limit)
	}
	if normalizedAgent(agent) == "omo" || normalizedAgent(agent) == "ohmyopencode" {
		return r.readOMO(project.CWD, sessionID, before, limit)
	}
	if !Supported(agent) {
		return unavailableCode("invalid_provider", "Conversation history is not available for this agent."), nil
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return unavailableCode("invalid_session", "This agent has not reported a conversation session yet."), nil
	}
	location := r.LocateWithProject(agent, project, sessionID)
	if location.Path == "" {
		return unavailableCode("invalid_session", "No conversation log is available for this session."), nil
	}
	if isClaudeProvider(agent) {
		return r.readClaudeChain(project.CWD, sessionID, location, before, limit)
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
	return unavailableCode("source_unavailable", reason)
}

func unavailableCode(code, reason string) Page {
	return Page{Available: false, ReasonCode: code, Reason: reason, Entries: []Entry{}}
}

// Locate returns the exact contained transcript selected for agent, cwd and
// sessionID. Root order is authoritative; within Claude roots the foreground
// directory is tried before the pane cwd. Cached tuple locations are returned
// before any filesystem walk, keeping title and history on the same copy.
func (r *Reader) Locate(agent, cwd, sessionID string) Location {
	return r.LocateWithProject(agent, ProjectContext{CWD: cwd}, sessionID)
}

// LocateWithProject returns the exact contained transcript selected for the
// supplied project context. The effective foreground hint is part of the
// cache and single-flight keys, so changing it cannot reuse an old hit or
// miss.
func (r *Reader) LocateWithProject(agent string, project ProjectContext, sessionID string) Location {
	if r != nil && r.installed != nil {
		location, _ := r.LocateWithProjectContext(context.Background(), agent, project, sessionID)
		return location
	}
	project = NormalizeProjectContext(agent, project)
	sessionID = strings.TrimSpace(sessionID)
	agentKey := normalizedAgent(agent)
	if isHermesAgent(agent) {
		agentKey = "hermes"
	}
	key := locationKey{Agent: agentKey, CWD: project.CWD, ForegroundCWD: project.ForegroundCWD, SessionID: sessionID}
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

	location := r.locateWithProject(agent, project, sessionID)
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

// locate preserves the old package-local helper shape for tests and legacy
// callers; public callers should use Locate or LocateWithProject.
func (r *Reader) locate(agent, cwd, sessionID string) Location {
	return r.locateWithProject(agent, ProjectContext{CWD: cwd}, sessionID)
}

func (r *Reader) locateWithProject(agent string, project ProjectContext, sessionID string) Location {
	project = NormalizeProjectContext(agent, project)
	switch normalizedAgent(agent) {
	case "claude", "claudecode":
		if !safeSessionID(sessionID) {
			return Location{}
		}
		return findProjectSessionWithProject(r.claudeRoots(), project, sessionID+".jsonl", func(cwd string) string {
			return claudeProjectNonAlphanumeric.ReplaceAllString(cwd, "-")
		})
	case "qoder", "qodercli":
		if !safeSessionID(sessionID) {
			return Location{}
		}
		return findProjectSessionWithProject(r.qoderRoots(), project, sessionID+".jsonl", func(string) string { return "" })
	case "codex", "openaicodex":
		if !canonicalSessionID.MatchString(sessionID) {
			return Location{}
		}
		return findCodexSession(r.codexRoots(), sessionID)
	case "pi", "picodingagent":
		return resolvePathOrSession(r.piRoots(), sessionID, "_")
	case "omp", "ohmypi":
		return resolvePathOrSession(r.ompRoots(), sessionID, "_")
	case "hermes", "hermesagent":
		return r.hermes.locate(project.CWD, sessionID)
	case "cursor", "cursoragent":
		return findCursorSession(r.cursorRoots(), project, sessionID)
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
	directory, _ := isDirContext(context.Background(), ordinaryReaderDiscoveryDeps(), path)
	return directory
}

func findProjectSession(roots []string, cwd, filename, preferredProjectName string) Location {
	return findProjectSessionWithProject(roots, ProjectContext{CWD: cwd}, filename, func(string) string {
		return preferredProjectName
	})
}

// findProjectSessionWithProject keeps roots as the outer loop. Within one root
// the more specific foreground directory is tried before the pane cwd, and an
// unknown-cwd scan is used only when neither known directory is available.
func findProjectSessionWithProject(roots []string, project ProjectContext, filename string, preferredProjectName func(string) string) Location {
	location, _ := findProjectSessionWithProjectContext(context.Background(), ordinaryReaderDiscoveryDeps(), roots, project, filename, preferredProjectName)
	return location
}

func projectDirectoriesForContext(project ProjectContext) []string {
	candidates := make([]string, 0, 2)
	if project.ForegroundCWD != "" {
		candidates = append(candidates, project.ForegroundCWD)
	}
	if strings.TrimSpace(project.CWD) != "" {
		if len(candidates) == 0 || candidates[0] != project.CWD {
			candidates = append(candidates, project.CWD)
		}
	}
	if len(candidates) == 0 {
		// An empty cwd has historical meaning: enumerate every project in the
		// root. It is deliberately not combined with a valid foreground hint.
		return []string{""}
	}
	return candidates
}

func projectDirectories(root, cwd, preferredProjectName string) []string {
	directories, _ := projectDirectoriesContext(context.Background(), ordinaryReaderDiscoveryDeps(), root, cwd, preferredProjectName)
	return directories
}

func findCodexSession(roots []string, sessionID string) Location {
	location, _ := findCodexSessionContext(context.Background(), ordinaryReaderDiscoveryDeps(), roots, sessionID)
	return location
}

func descendingDirectories(path string) []string {
	directories, _ := descendingDirectoriesContext(context.Background(), ordinaryReaderDiscoveryDeps(), path)
	return directories
}

func resolvePathOrSession(roots []string, sessionID, separator string) Location {
	location, _ := resolvePathOrSessionContext(context.Background(), ordinaryReaderDiscoveryDeps(), roots, sessionID, separator)
	return location
}

func containedRegularFile(path, root string) string {
	contained, _ := containedRegularFileContext(context.Background(), ordinaryReaderDiscoveryDeps(), path, root)
	return contained
}

func loadTail(path string, limit int64) (string, bool, error) {
	file, err := openConversationSource(path)
	if err != nil {
		return "", false, err
	}
	defer file.Close()
	return loadTailFile(file, limit)
}

func loadTailFile(file *os.File, limit int64) (string, bool, error) {
	if file == nil {
		return "", false, errors.New("conversation source is closed")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		if err == nil {
			err = errors.New("conversation source is not a regular file")
		}
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
	entries, _ := parseTranscriptContext(context.Background(), agent, text)
	return entries
}

func parseToolActivity(agent string, record map[string]any) ([]ToolActivity, []toolResult) {
	calls, results, _ := parseToolActivityContext(context.Background(), agent, record)
	return calls, results
}

func toolsFromBlocks(blocks []any) ([]ToolActivity, []toolResult) {
	calls, results, _ := toolsFromBlocksContext(context.Background(), blocks)
	return calls, results
}

func newToolActivity(id, name string, input any) ToolActivity {
	tool, _ := newToolActivityContext(context.Background(), id, name, input)
	return tool
}

func normalizeToolID(value string) string {
	value, _ = normalizeToolIDContext(context.Background(), value)
	return value
}

func toolAssociationID(tool ToolActivity) string {
	if tool.associationID != "" {
		return tool.associationID
	}
	return strings.TrimSpace(tool.ID)
}

func toolAssociationKey(agent, id string) string {
	digest := sha256.Sum256([]byte(agent + "\x00" + id))
	return hex.EncodeToString(digest[:])
}

func normalizeToolActivity(tool ToolActivity) (ToolActivity, bool) {
	tool, changed, _ := normalizeToolActivityContext(context.Background(), tool)
	return tool, changed
}

func normalizeEntryTools(entry *Entry) (int, int) {
	tools, payloads, _ := normalizeEntryToolsContext(context.Background(), entry)
	return tools, payloads
}

func normalizeEntriesForResponse(entries []Entry) BrowseDiagnostics {
	diagnostics, _ := normalizeEntriesForResponseContext(context.Background(), entries)
	return diagnostics
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
	text, _ := textValueContext(context.Background(), value)
	return text
}

func parseClaudeRecord(record map[string]any) (string, string) {
	role, text, _ := parseClaudeRecordContext(context.Background(), record)
	return role, text
}

var (
	bashOutputRow     = regexp.MustCompile(`(?s)^<bash-stdout>.*</bash-stdout><bash-stderr>.*</bash-stderr>$`)
	bashOutputElement = regexp.MustCompile(`(?s)<bash-stdout>.*?</bash-stdout>|<bash-stderr>.*?</bash-stderr>`)
)

var commandTags = regexp.MustCompile(`(?s)<command-(?:message|name|args)>.*?</command-(?:message|name|args)>`)

func humanClaudeText(raw string) string {
	trimmed := strings.TrimSpace(raw)
	for _, envelope := range []string{"<system-reminder>", "<local-command-caveat>", "<task-notification>", "<local-command-stdout>"} {
		if strings.HasPrefix(trimmed, envelope) {
			return ""
		}
	}
	// Shell output rows: Claude Code writes stdout and stderr together as one
	// row, and output may itself contain tag text, so the genuine adjacent pair is
	// recognised first. A row made only of output elements is hidden too; a
	// prompt that also carries other text keeps all of it.
	if strings.HasPrefix(trimmed, "<bash-stdout>") || strings.HasPrefix(trimmed, "<bash-stderr>") {
		if bashOutputRow.MatchString(trimmed) || strings.TrimSpace(bashOutputElement.ReplaceAllString(trimmed, "")) == "" {
			return ""
		}
		return raw
	}
	// A shell command row is exactly one bash-input element; a prompt holding
	// several keeps its text.
	if strings.HasPrefix(trimmed, "<bash-input>") && strings.HasSuffix(trimmed, "</bash-input>") && strings.Count(trimmed, "<bash-input>") == 1 {
		return "! " + strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(trimmed, "<bash-input>"), "</bash-input>"))
	}
	// A slash-command row is nothing but command-* elements; a prompt that also
	// carries other text keeps all of it. A row without a command name is noise.
	if strings.HasPrefix(trimmed, "<command-name>") || strings.HasPrefix(trimmed, "<command-message>") {
		if strings.TrimSpace(commandTags.ReplaceAllString(trimmed, "")) == "" {
			name := innerTag(trimmed, "command-name")
			if name == "" {
				return ""
			}
			return strings.TrimSpace(name + " " + innerTag(trimmed, "command-args"))
		}
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
	role, text, _ := parseCodexRecordContext(context.Background(), record)
	return role, text
}

func parsePiRecord(record map[string]any) (string, string) {
	role, text, _ := parsePiRecordContext(context.Background(), record)
	return role, text
}

func textBlocks(value any) string {
	text, _ := textBlocksContext(context.Background(), value)
	return text
}

// textBlockList reports the text carried by each text-like block separately.
// Callers that filter per block need the boundaries: joining first would let an
// envelope in a later block escape a prefix test anchored on the whole string.
func textBlockList(value any) []string {
	texts, _ := textBlockListContext(context.Background(), value)
	return texts
}

func sanitizeText(text string) string {
	text, _ = sanitizeTextContext(context.Background(), text)
	return text
}

func clampText(text string, limit int) (string, bool) {
	text, truncated, _ := clampTextContext(context.Background(), text, limit)
	return text, truncated
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

// readerParseContextErr supplies cancellation only, not operation admission.
func readerParseContextErr(ctx context.Context) error {
	if ctx == nil {
		return conversationlife.ErrRefused
	}
	return ctx.Err()
}

func textValueContext(ctx context.Context, value any) (text string, err error) {
	if err = readerParseContextErr(ctx); err != nil {
		return "", err
	}
	defer func() {
		if finalErr := readerParseContextErr(ctx); finalErr != nil {
			err = finalErr
		}
		if err != nil {
			text = ""
		}
	}()
	if raw, ok := value.(string); ok {
		return raw, nil
	}
	text, err = textBlocksContext(ctx, value)
	if err != nil {
		return "", err
	}
	if err = readerParseContextErr(ctx); err != nil {
		return "", err
	}
	if text != "" {
		return text, nil
	}
	data, marshalErr := json.Marshal(value)
	if err = readerParseContextErr(ctx); err != nil {
		return "", err
	}
	if marshalErr != nil || string(data) == "null" {
		return "", nil
	}
	return string(data), nil
}

func textBlockListContext(ctx context.Context, value any) (texts []string, err error) {
	if err = readerParseContextErr(ctx); err != nil {
		return nil, err
	}
	defer func() {
		if finalErr := readerParseContextErr(ctx); finalErr != nil {
			err = finalErr
		}
		if err != nil {
			texts = nil
		}
	}()
	blocks, ok := value.([]any)
	if !ok {
		return nil, nil
	}
	texts = make([]string, 0, len(blocks))
	for _, rawBlock := range blocks {
		if err = readerParseContextErr(ctx); err != nil {
			return nil, err
		}
		block, ok := rawBlock.(map[string]any)
		if !ok {
			continue
		}
		typeName := stringValue(block["type"])
		if err = readerParseContextErr(ctx); err != nil {
			return nil, err
		}
		if typeName != "text" && typeName != "input_text" && typeName != "output_text" {
			continue
		}
		text := stringValue(block["text"])
		if err = readerParseContextErr(ctx); err != nil {
			return nil, err
		}
		if text != "" {
			texts = append(texts, text)
		}
	}
	return texts, nil
}

func textBlocksContext(ctx context.Context, value any) (text string, err error) {
	if err = readerParseContextErr(ctx); err != nil {
		return "", err
	}
	defer func() {
		if finalErr := readerParseContextErr(ctx); finalErr != nil {
			err = finalErr
		}
		if err != nil {
			text = ""
		}
	}()
	if raw, ok := value.(string); ok {
		return raw, nil
	}
	blocks, err := textBlockListContext(ctx, value)
	if err != nil {
		return "", err
	}
	if err = readerParseContextErr(ctx); err != nil {
		return "", err
	}
	return strings.Join(blocks, "\n"), nil
}

func parseClaudeRecordContext(ctx context.Context, record map[string]any) (role string, text string, err error) {
	if err = readerParseContextErr(ctx); err != nil {
		return "", "", err
	}
	defer func() {
		if finalErr := readerParseContextErr(ctx); finalErr != nil {
			err = finalErr
		}
		if err != nil {
			role, text = "", ""
		}
	}()
	role = stringValue(record["type"])
	if err = readerParseContextErr(ctx); err != nil {
		return "", "", err
	}
	if (role != "user" && role != "assistant") || record["isSidechain"] == true {
		return "", "", nil
	}
	if role == "user" && (record["isMeta"] == true || record["isCompactSummary"] == true) {
		return "", "", nil
	}
	message, ok := record["message"].(map[string]any)
	if !ok {
		return "", "", nil
	}
	content := message["content"]
	if err = readerParseContextErr(ctx); err != nil {
		return "", "", err
	}
	if raw, ok := content.(string); ok {
		if role != "user" {
			return role, raw, nil
		}
		return role, humanClaudeText(raw), nil
	}
	if role == "user" {
		blocks, blockErr := textBlockListContext(ctx, content)
		if blockErr != nil {
			return "", "", blockErr
		}
		if err = readerParseContextErr(ctx); err != nil {
			return "", "", err
		}
		kept := make([]string, 0, len(blocks))
		// Filter per block, not on the joined string: humanClaudeText anchors its
		// envelope checks on the start of what it is given, so joining first
		// would let a <system-reminder> in any block after the first survive
		// into the phone's conversation view.
		for _, block := range blocks {
			if err = readerParseContextErr(ctx); err != nil {
				return "", "", err
			}
			filtered := humanClaudeText(block)
			if err = readerParseContextErr(ctx); err != nil {
				return "", "", err
			}
			nonempty := strings.TrimSpace(filtered) != ""
			if err = readerParseContextErr(ctx); err != nil {
				return "", "", err
			}
			if nonempty {
				kept = append(kept, filtered)
			}
		}
		if err = readerParseContextErr(ctx); err != nil {
			return "", "", err
		}
		if len(kept) == 0 {
			return "", "", nil
		}
		return role, strings.Join(kept, "\n"), nil
	}
	text, err = textBlocksContext(ctx, content)
	return role, text, err
}

func parseCodexRecordContext(ctx context.Context, record map[string]any) (role string, text string, err error) {
	if err = readerParseContextErr(ctx); err != nil {
		return "", "", err
	}
	defer func() {
		if finalErr := readerParseContextErr(ctx); finalErr != nil {
			err = finalErr
		}
		if err != nil {
			role, text = "", ""
		}
	}()
	typeName := stringValue(record["type"])
	if err = readerParseContextErr(ctx); err != nil {
		return "", "", err
	}
	if typeName != "response_item" {
		return "", "", nil
	}
	payload, ok := record["payload"].(map[string]any)
	if !ok {
		return "", "", nil
	}
	typeName = stringValue(payload["type"])
	if err = readerParseContextErr(ctx); err != nil {
		return "", "", err
	}
	if typeName != "message" {
		return "", "", nil
	}
	role = stringValue(payload["role"])
	if err = readerParseContextErr(ctx); err != nil {
		return "", "", err
	}
	if role != "user" && role != "assistant" {
		return "", "", nil
	}
	text, err = textBlocksContext(ctx, payload["content"])
	if err != nil {
		return "", "", err
	}
	if err = readerParseContextErr(ctx); err != nil {
		return "", "", err
	}
	if role == "user" {
		trimmed := strings.TrimSpace(text)
		if err = readerParseContextErr(ctx); err != nil {
			return "", "", err
		}
		if strings.HasPrefix(trimmed, "<environment_context>") {
			return "", "", nil
		}
	}
	return role, text, nil
}

func parsePiRecordContext(ctx context.Context, record map[string]any) (role string, text string, err error) {
	if err = readerParseContextErr(ctx); err != nil {
		return "", "", err
	}
	defer func() {
		if finalErr := readerParseContextErr(ctx); finalErr != nil {
			err = finalErr
		}
		if err != nil {
			role, text = "", ""
		}
	}()
	typeName := stringValue(record["type"])
	if err = readerParseContextErr(ctx); err != nil {
		return "", "", err
	}
	if typeName != "message" {
		return "", "", nil
	}
	message, ok := record["message"].(map[string]any)
	if !ok {
		return "", "", nil
	}
	role = stringValue(message["role"])
	if err = readerParseContextErr(ctx); err != nil {
		return "", "", err
	}
	if role != "user" && role != "assistant" {
		return "", "", nil
	}
	text, err = textBlocksContext(ctx, message["content"])
	return role, text, err
}

// parseTranscriptContext owns its entry/tool buffers; errors publish no partial transcript.
func parseTranscriptContext(ctx context.Context, agent, text string) (entries []Entry, err error) {
	if err = readerParseContextErr(ctx); err != nil {
		return nil, err
	}
	defer func() {
		if finalErr := readerParseContextErr(ctx); finalErr != nil {
			err = finalErr
		}
		if err != nil {
			entries = nil
		}
	}()
	normalized := normalizedAgent(agent)
	if err = readerParseContextErr(ctx); err != nil {
		return nil, err
	}
	entries = make([]Entry, 0)
	seenIDs := make(map[string]int)
	pendingTools := make(map[string]toolLocation)
	lines := strings.Split(text, "\n")
	if err = readerParseContextErr(ctx); err != nil {
		return nil, err
	}
	for _, line := range lines {
		if err = readerParseContextErr(ctx); err != nil {
			return nil, err
		}
		blank := strings.TrimSpace(line) == ""
		if err = readerParseContextErr(ctx); err != nil {
			return nil, err
		}
		if blank {
			continue
		}
		var record map[string]any
		decodeErr := json.Unmarshal([]byte(line), &record)
		if err = readerParseContextErr(ctx); err != nil {
			return nil, err
		}
		if decodeErr != nil {
			continue
		}
		calls, results, toolErr := parseToolActivityContext(ctx, normalized, record)
		if toolErr != nil {
			return nil, toolErr
		}
		for _, result := range results {
			if err = readerParseContextErr(ctx); err != nil {
				return nil, err
			}
			key := toolAssociationKey(normalized, result.id)
			if err = readerParseContextErr(ctx); err != nil {
				return nil, err
			}
			location, ok := pendingTools[key]
			if !ok || location.entry >= len(entries) || location.tool >= len(entries[location.entry].Tools) {
				continue
			}
			output, outputErr := sanitizeTextContext(ctx, result.output)
			if outputErr != nil {
				return nil, outputErr
			}
			output, truncated, outputErr := clampTextContext(ctx, output, maxEntryBytes)
			if outputErr != nil {
				return nil, outputErr
			}
			if err = readerParseContextErr(ctx); err != nil {
				return nil, err
			}
			tool := &entries[location.entry].Tools[location.tool]
			tool.Output = output
			tool.Error = result.failed
			tool.Truncated = tool.Truncated || truncated
			delete(pendingTools, key)
		}
		if err = readerParseContextErr(ctx); err != nil {
			return nil, err
		}
		role, timestamp, body := "", stringValue(record["timestamp"]), ""
		if err = readerParseContextErr(ctx); err != nil {
			return nil, err
		}
		switch normalized {
		case "claude", "claudecode", "qoder", "qodercli":
			role, body, err = parseClaudeRecordContext(ctx, record)
		case "codex", "openaicodex":
			role, body, err = parseCodexRecordContext(ctx, record)
		case "pi", "picodingagent", "omp", "ohmypi", "omo", "ohmyopencode":
			role, body, err = parsePiRecordContext(ctx, record)
		case "cursor", "cursoragent":
			role, body, err = parseCursorRecordContext(ctx, record)
			if err != nil {
				return nil, err
			}
			if timestamp == "" {
				timestamp, err = cursorTimestampFromRecordContext(ctx, record)
			}
		}
		if err != nil {
			return nil, err
		}
		body, err = sanitizeTextContext(ctx, body)
		if err != nil {
			return nil, err
		}
		if role == "" && len(calls) > 0 {
			role = "assistant"
		}
		if role == "" || (body == "" && len(calls) == 0) {
			continue
		}
		body, truncated, clampErr := clampTextContext(ctx, body, maxEntryBytes)
		if clampErr != nil {
			return nil, clampErr
		}
		entry := Entry{Timestamp: timestamp, Role: role, Text: body, Tools: calls, Truncated: truncated}
		if _, _, err = normalizeEntryToolsContext(ctx, &entry); err != nil {
			return nil, err
		}
		if err = readerParseContextErr(ctx); err != nil {
			return nil, err
		}
		entry.ID = stableRowID(line, seenIDs)
		if err = readerParseContextErr(ctx); err != nil {
			return nil, err
		}
		entryIndex := len(entries)
		entries = append(entries, entry)
		// Keep registration over original calls, including indexes beyond the cap.
		for toolIndex := range calls {
			if err = readerParseContextErr(ctx); err != nil {
				return nil, err
			}
			id := toolAssociationID(calls[toolIndex])
			if err = readerParseContextErr(ctx); err != nil {
				return nil, err
			}
			if id != "" {
				key := toolAssociationKey(normalized, id)
				if err = readerParseContextErr(ctx); err != nil {
					return nil, err
				}
				pendingTools[key] = toolLocation{entry: entryIndex, tool: toolIndex}
			}
		}
	}
	return entries, nil
}

func parseToolActivityContext(ctx context.Context, agent string, record map[string]any) (calls []ToolActivity, results []toolResult, err error) {
	if err = readerParseContextErr(ctx); err != nil {
		return nil, nil, err
	}
	defer func() {
		if finalErr := readerParseContextErr(ctx); finalErr != nil {
			err = finalErr
		}
		if err != nil {
			calls, results = nil, nil
		}
	}()
	switch agent {
	case "claude", "claudecode", "qoder", "qodercli":
		if record["isSidechain"] == true {
			return nil, nil, nil
		}
		message, _ := record["message"].(map[string]any)
		blocks, _ := message["content"].([]any)
		return toolsFromBlocksContext(ctx, blocks)
	case "codex", "openaicodex":
		typeName := stringValue(record["type"])
		if err = readerParseContextErr(ctx); err != nil {
			return nil, nil, err
		}
		if typeName != "response_item" {
			return nil, nil, nil
		}
		payload, _ := record["payload"].(map[string]any)
		typeName = normalizedBlockType(payload["type"])
		if err = readerParseContextErr(ctx); err != nil {
			return nil, nil, err
		}
		switch typeName {
		case "functioncall", "customtoolcall", "localshellcall":
			id := firstString(payload, "call_id", "id")
			if err = readerParseContextErr(ctx); err != nil {
				return nil, nil, err
			}
			name := firstString(payload, "name", "tool_name")
			if err = readerParseContextErr(ctx); err != nil {
				return nil, nil, err
			}
			input := firstValue(payload, "arguments", "input")
			call, callErr := newToolActivityContext(ctx, id, name, input)
			if callErr != nil {
				return nil, nil, callErr
			}
			return []ToolActivity{call}, nil, nil
		case "functioncalloutput", "customtoolcalloutput", "localshellcalloutput":
			id := firstString(payload, "call_id", "id")
			if err = readerParseContextErr(ctx); err != nil {
				return nil, nil, err
			}
			id = strings.TrimSpace(id)
			if err = readerParseContextErr(ctx); err != nil {
				return nil, nil, err
			}
			value := firstValue(payload, "output", "content")
			output, outputErr := textValueContext(ctx, value)
			if outputErr != nil {
				return nil, nil, outputErr
			}
			return nil, []toolResult{{id: id, output: output, failed: payload["is_error"] == true}}, nil
		}
	case "pi", "picodingagent", "omp", "ohmypi", "omo", "ohmyopencode":
		typeName := stringValue(record["type"])
		if err = readerParseContextErr(ctx); err != nil {
			return nil, nil, err
		}
		if typeName != "message" {
			return nil, nil, nil
		}
		message, _ := record["message"].(map[string]any)
		typeName = normalizedBlockType(message["role"])
		if err = readerParseContextErr(ctx); err != nil {
			return nil, nil, err
		}
		if typeName == "toolresult" {
			id := firstString(message, "toolCallId", "tool_call_id", "id")
			if err = readerParseContextErr(ctx); err != nil {
				return nil, nil, err
			}
			id = strings.TrimSpace(id)
			output, outputErr := textValueContext(ctx, message["content"])
			if outputErr != nil {
				return nil, nil, outputErr
			}
			return nil, []toolResult{{id: id, output: output, failed: message["isError"] == true || message["is_error"] == true}}, nil
		}
		blocks, _ := message["content"].([]any)
		return toolsFromBlocksContext(ctx, blocks)
	case "cursor", "cursoragent":
		message, _ := record["message"].(map[string]any)
		blocks, _ := message["content"].([]any)
		return toolsFromBlocksContext(ctx, blocks)
	}
	return nil, nil, nil
}

func toolsFromBlocksContext(ctx context.Context, blocks []any) (calls []ToolActivity, results []toolResult, err error) {
	if err = readerParseContextErr(ctx); err != nil {
		return nil, nil, err
	}
	defer func() {
		if finalErr := readerParseContextErr(ctx); finalErr != nil {
			err = finalErr
		}
		if err != nil {
			calls, results = nil, nil
		}
	}()
	calls = make([]ToolActivity, 0)
	results = make([]toolResult, 0)
	for _, raw := range blocks {
		if err = readerParseContextErr(ctx); err != nil {
			return nil, nil, err
		}
		block, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		typeName := normalizedBlockType(block["type"])
		if err = readerParseContextErr(ctx); err != nil {
			return nil, nil, err
		}
		switch typeName {
		case "tooluse", "toolcall":
			id := firstString(block, "id", "toolCallId", "tool_call_id")
			if err = readerParseContextErr(ctx); err != nil {
				return nil, nil, err
			}
			name := firstString(block, "name", "toolName", "tool_name")
			if err = readerParseContextErr(ctx); err != nil {
				return nil, nil, err
			}
			input := firstValue(block, "input", "arguments")
			call, callErr := newToolActivityContext(ctx, id, name, input)
			if callErr != nil {
				return nil, nil, callErr
			}
			calls = append(calls, call)
		case "toolresult":
			id := firstString(block, "tool_use_id", "toolCallId", "tool_call_id", "id")
			if err = readerParseContextErr(ctx); err != nil {
				return nil, nil, err
			}
			id = strings.TrimSpace(id)
			output, outputErr := textValueContext(ctx, block["content"])
			if outputErr != nil {
				return nil, nil, outputErr
			}
			results = append(results, toolResult{
				id: id, output: output, failed: block["is_error"] == true || block["isError"] == true,
			})
		}
	}
	return calls, results, nil
}

func newToolActivityContext(ctx context.Context, id, name string, input any) (tool ToolActivity, err error) {
	if err = readerParseContextErr(ctx); err != nil {
		return ToolActivity{}, err
	}
	defer func() {
		if finalErr := readerParseContextErr(ctx); finalErr != nil {
			err = finalErr
		}
		if err != nil {
			tool = ToolActivity{}
		}
	}()
	if strings.TrimSpace(name) == "" {
		name = "Tool"
	}
	inputText, err := textValueContext(ctx, input)
	if err != nil {
		return ToolActivity{}, err
	}
	inputText, err = sanitizeTextContext(ctx, inputText)
	if err != nil {
		return ToolActivity{}, err
	}
	inputText, truncated, err := clampTextContext(ctx, inputText, maxEntryBytes/2)
	if err != nil {
		return ToolActivity{}, err
	}
	id = strings.TrimSpace(id)
	if err = readerParseContextErr(ctx); err != nil {
		return ToolActivity{}, err
	}
	name = strings.TrimSpace(name)
	if err = readerParseContextErr(ctx); err != nil {
		return ToolActivity{}, err
	}
	tool = ToolActivity{ID: id, Name: name, Input: inputText, Truncated: truncated, associationID: id}
	tool, _, err = normalizeToolActivityContext(ctx, tool)
	return tool, err
}

func normalizeToolIDContext(ctx context.Context, value string) (id string, err error) {
	if err = readerParseContextErr(ctx); err != nil {
		return "", err
	}
	defer func() {
		if finalErr := readerParseContextErr(ctx); finalErr != nil {
			err = finalErr
		}
		if err != nil {
			id = ""
		}
	}()
	value = strings.TrimSpace(value)
	id, _, err = clampTextContext(ctx, value, maxToolIDBytes)
	return id, err
}

func normalizeToolActivityContext(ctx context.Context, tool ToolActivity) (normalized ToolActivity, changed bool, err error) {
	if err = readerParseContextErr(ctx); err != nil {
		return ToolActivity{}, false, err
	}
	defer func() {
		if finalErr := readerParseContextErr(ctx); finalErr != nil {
			err = finalErr
		}
		if err != nil {
			normalized, changed = ToolActivity{}, false
		}
	}()
	originalID, originalName := tool.ID, tool.Name
	originalInput, originalOutput := tool.Input, tool.Output
	originalError, originalTruncated := tool.Error, tool.Truncated
	associationID := toolAssociationID(tool)
	if err = readerParseContextErr(ctx); err != nil {
		return ToolActivity{}, false, err
	}
	tool.associationID = associationID
	tool.ID, err = normalizeToolIDContext(ctx, associationID)
	if err != nil {
		return ToolActivity{}, false, err
	}
	if err = readerParseContextErr(ctx); err != nil {
		return ToolActivity{}, false, err
	}
	name := strings.TrimSpace(tool.Name)
	tool.Name, _, err = clampTextContext(ctx, name, maxToolNameBytes)
	if err != nil {
		return ToolActivity{}, false, err
	}
	tool.Input, _, err = clampTextContext(ctx, tool.Input, maxToolInputBytes)
	if err != nil {
		return ToolActivity{}, false, err
	}
	tool.Output, _, err = clampTextContext(ctx, tool.Output, maxToolOutputBytes)
	if err != nil {
		return ToolActivity{}, false, err
	}
	if tool.Name == "" {
		tool.Name = "Tool"
	}
	changed = tool.ID != originalID || tool.Name != originalName || tool.Input != originalInput ||
		tool.Output != originalOutput || tool.Error != originalError || tool.Truncated != originalTruncated
	if changed {
		tool.Truncated = true
	}
	return tool, changed, nil
}

// normalizeEntryToolsContext mutates caller-owned private scratch in place, retaining
// backing unless the count cap requires a copy. On error scratch may be partially
// normalized, but returned counts are zero. Installed callers must independently
// copy inputs/cache storage first and discard scratch on error; no rollback is promised.
func normalizeEntryToolsContext(ctx context.Context, entry *Entry) (omittedTools int, omittedPayloads int, err error) {
	if err = readerParseContextErr(ctx); err != nil {
		return 0, 0, err
	}
	defer func() {
		if finalErr := readerParseContextErr(ctx); finalErr != nil {
			err = finalErr
		}
		if err != nil {
			omittedTools, omittedPayloads = 0, 0
		}
	}()
	if entry == nil || len(entry.Tools) == 0 {
		return 0, 0, nil
	}
	if len(entry.Tools) > maxToolCount {
		omittedTools = len(entry.Tools) - maxToolCount
		entry.Tools = append([]ToolActivity(nil), entry.Tools[:maxToolCount]...)
		entry.Truncated = true
	}
	for index, tool := range entry.Tools {
		if err = readerParseContextErr(ctx); err != nil {
			return 0, 0, err
		}
		normalized, changed, normalizeErr := normalizeToolActivityContext(ctx, tool)
		if normalizeErr != nil {
			return 0, 0, normalizeErr
		}
		if err = readerParseContextErr(ctx); err != nil {
			return 0, 0, err
		}
		if changed || normalized.Truncated {
			omittedPayloads++
			entry.Truncated = true
		}
		entry.Tools[index] = normalized
	}
	return omittedTools, omittedPayloads, nil
}

// normalizeEntriesForResponseContext has the same private-scratch contract as
// normalizeEntryToolsContext: zero diagnostics on error, not mutation rollback.
func normalizeEntriesForResponseContext(ctx context.Context, entries []Entry) (diagnostics BrowseDiagnostics, err error) {
	if err = readerParseContextErr(ctx); err != nil {
		return BrowseDiagnostics{}, err
	}
	defer func() {
		if finalErr := readerParseContextErr(ctx); finalErr != nil {
			err = finalErr
		}
		if err != nil {
			diagnostics = BrowseDiagnostics{}
		}
	}()
	for index := range entries {
		if err = readerParseContextErr(ctx); err != nil {
			return BrowseDiagnostics{}, err
		}
		tools, payloads, normalizeErr := normalizeEntryToolsContext(ctx, &entries[index])
		if normalizeErr != nil {
			return BrowseDiagnostics{}, normalizeErr
		}
		diagnostics.OmittedTools += tools
		diagnostics.OmittedPayloads += payloads
	}
	return diagnostics, nil
}

func sanitizeTextContext(ctx context.Context, text string) (clean string, err error) {
	if err = readerParseContextErr(ctx); err != nil {
		return "", err
	}
	defer func() {
		if finalErr := readerParseContextErr(ctx); finalErr != nil {
			err = finalErr
		}
		if err != nil {
			clean = ""
		}
	}()
	text = panehistory.NormalizeLine(text)
	if err = readerParseContextErr(ctx); err != nil {
		return "", err
	}
	text = strings.ReplaceAll(text, "\x00", "")
	if err = readerParseContextErr(ctx); err != nil {
		return "", err
	}
	return strings.TrimSpace(text), nil
}

func clampTextContext(ctx context.Context, text string, limit int) (clipped string, truncated bool, err error) {
	if err = readerParseContextErr(ctx); err != nil {
		return "", false, err
	}
	defer func() {
		if finalErr := readerParseContextErr(ctx); finalErr != nil {
			err = finalErr
		}
		if err != nil {
			clipped, truncated = "", false
		}
	}()
	if len(text) <= limit {
		return text, false, nil
	}
	clipped = text[:limit]
	for {
		if err = readerParseContextErr(ctx); err != nil {
			return "", false, err
		}
		valid := utf8.ValidString(clipped)
		if err = readerParseContextErr(ctx); err != nil {
			return "", false, err
		}
		if valid {
			break
		}
		clipped = clipped[:len(clipped)-1]
	}
	return strings.TrimSpace(clipped), true, nil
}

// Discovery dependencies describe effects, not installed operation authority.
type readerDiscoveryDeps struct {
	stat        func(string) (os.FileInfo, error)
	eval        func(string) (string, error)
	readDir     func(string) ([]os.DirEntry, error)
	readContent func(context.Context, Location) ([]byte, error)
}

func (d readerDiscoveryDeps) complete() bool {
	return d.stat != nil && d.eval != nil && d.readDir != nil && d.readContent != nil
}

// Installed callers must instead supply owned content under their live same-owner Call.
func ordinaryReaderDiscoveryDeps() readerDiscoveryDeps {
	return readerDiscoveryDeps{
		stat: os.Stat, eval: filepath.EvalSymlinks, readDir: os.ReadDir,
		readContent: func(_ context.Context, location Location) ([]byte, error) {
			return os.ReadFile(location.Path)
		},
	}
}

func readerDiscoveryResult(ctx context.Context, err error) error {
	cancelled := readerParseContextErr(ctx)
	if cancelled == nil {
		return err
	}
	if err == nil {
		return cancelled
	}
	if errors.Is(err, cancelled) {
		return err
	}
	return errors.Join(err, cancelled)
}

func isDirContext(ctx context.Context, d readerDiscoveryDeps, path string) (directory bool, err error) {
	defer func() {
		err = readerDiscoveryResult(ctx, err)
		if err != nil {
			directory = false
		}
	}()
	if err = readerParseContextErr(ctx); err != nil {
		return false, err
	}
	if !d.complete() {
		return false, conversationlife.ErrRefused
	}
	var info os.FileInfo
	err = sourceEffect(ctx, func() (effectErr error) { info, effectErr = d.stat(path); return })
	if terminalProviderError(err) {
		return false, err
	}
	if err != nil || info == nil {
		return false, nil
	}
	return info.IsDir(), nil
}

func findProjectSessionWithProjectContext(ctx context.Context, d readerDiscoveryDeps, roots []string, project ProjectContext, filename string, preferredProjectName func(string) string) (location Location, err error) {
	defer func() {
		err = readerDiscoveryResult(ctx, err)
		if err != nil {
			location = Location{}
		}
	}()
	if err = readerParseContextErr(ctx); err != nil {
		return Location{}, err
	}
	if !d.complete() {
		return Location{}, conversationlife.ErrRefused
	}
	candidates := projectDirectoriesForContext(project)
	if err = readerParseContextErr(ctx); err != nil {
		return Location{}, err
	}
	for _, root := range roots {
		if err = readerParseContextErr(ctx); err != nil {
			return Location{}, err
		}
		seen := make(map[string]bool)
		for _, cwd := range candidates {
			if err = readerParseContextErr(ctx); err != nil {
				return Location{}, err
			}
			preferred := ""
			if preferredProjectName != nil {
				preferred = preferredProjectName(cwd)
				if err = readerParseContextErr(ctx); err != nil {
					return Location{}, err
				}
			}
			directories, discoveryErr := projectDirectoriesContext(ctx, d, root, cwd, preferred)
			if discoveryErr != nil {
				return Location{}, discoveryErr
			}
			if err = readerParseContextErr(ctx); err != nil {
				return Location{}, err
			}
			for _, projectDir := range directories {
				if err = readerParseContextErr(ctx); err != nil {
					return Location{}, err
				}
				projectDir = filepath.Clean(projectDir)
				if err = readerParseContextErr(ctx); err != nil {
					return Location{}, err
				}
				if seen[projectDir] {
					continue
				}
				seen[projectDir] = true
				candidate := filepath.Join(projectDir, filename)
				if err = readerParseContextErr(ctx); err != nil {
					return Location{}, err
				}
				path, discoveryErr := containedRegularFileContext(ctx, d, candidate, root)
				if discoveryErr != nil {
					return Location{}, discoveryErr
				}
				if err = readerParseContextErr(ctx); err != nil {
					return Location{}, err
				}
				if path != "" {
					return Location{Path: path, Root: root}, nil
				}
			}
		}
	}
	return Location{}, nil
}

func projectDirectoriesContext(ctx context.Context, d readerDiscoveryDeps, root, cwd, preferredProjectName string) (directories []string, err error) {
	defer func() {
		err = readerDiscoveryResult(ctx, err)
		if err != nil {
			directories = nil
		}
	}()
	if err = readerParseContextErr(ctx); err != nil {
		return nil, err
	}
	if !d.complete() {
		return nil, conversationlife.ErrRefused
	}
	var entries []os.DirEntry
	err = sourceEffect(ctx, func() (effectErr error) { entries, effectErr = d.readDir(root); return })
	if terminalProviderError(err) {
		return nil, err
	}
	if err != nil {
		return nil, nil
	}
	emptyCWD := strings.TrimSpace(cwd) == ""
	if err = readerParseContextErr(ctx); err != nil {
		return nil, err
	}
	if emptyCWD {
		directories = make([]string, 0, len(entries))
		for _, entry := range entries {
			if err = readerParseContextErr(ctx); err != nil {
				return nil, err
			}
			path := filepath.Join(root, entry.Name())
			if err = readerParseContextErr(ctx); err != nil {
				return nil, err
			}
			directory, discoveryErr := isDirContext(ctx, d, path)
			if discoveryErr != nil {
				return nil, discoveryErr
			}
			if err = readerParseContextErr(ctx); err != nil {
				return nil, err
			}
			if directory {
				directories = append(directories, path)
			}
		}
		return directories, nil
	}
	encoded := strings.ReplaceAll(strings.TrimPrefix(cwd, "/"), "/", "-")
	if err = readerParseContextErr(ctx); err != nil {
		return nil, err
	}
	candidates := [...]string{preferredProjectName, "-" + encoded, encoded}
	directories = make([]string, 0, len(candidates))
	seen := make(map[string]bool, len(candidates))
	for _, name := range candidates {
		if err = readerParseContextErr(ctx); err != nil {
			return nil, err
		}
		if name == "" {
			continue
		}
		path := filepath.Join(root, name)
		if err = readerParseContextErr(ctx); err != nil {
			return nil, err
		}
		if seen[path] {
			continue
		}
		directory, discoveryErr := isDirContext(ctx, d, path)
		if discoveryErr != nil {
			return nil, discoveryErr
		}
		if err = readerParseContextErr(ctx); err != nil {
			return nil, err
		}
		if !directory {
			continue
		}
		directories = append(directories, path)
		seen[path] = true
	}
	for _, entry := range entries {
		if err = readerParseContextErr(ctx); err != nil {
			return nil, err
		}
		path := filepath.Join(root, entry.Name())
		if err = readerParseContextErr(ctx); err != nil {
			return nil, err
		}
		if seen[path] {
			continue
		}
		directory, discoveryErr := isDirContext(ctx, d, path)
		if discoveryErr != nil {
			return nil, discoveryErr
		}
		if err = readerParseContextErr(ctx); err != nil {
			return nil, err
		}
		if !directory {
			continue
		}
		candidate := filepath.Join(path, "cwd")
		if err = readerParseContextErr(ctx); err != nil {
			return nil, err
		}
		cwdFile, discoveryErr := containedRegularFileContext(ctx, d, candidate, root)
		if discoveryErr != nil {
			return nil, discoveryErr
		}
		if err = readerParseContextErr(ctx); err != nil {
			return nil, err
		}
		if cwdFile == "" {
			continue
		}
		var data []byte
		readErr := sourceEffect(ctx, func() (effectErr error) {
			data, effectErr = d.readContent(ctx, Location{Path: cwdFile, Root: root})
			return
		})
		if terminalProviderError(readErr) {
			return nil, readErr
		}
		if readErr != nil {
			continue
		}
		matches := strings.TrimSpace(string(data)) == cwd
		if err = readerParseContextErr(ctx); err != nil {
			return nil, err
		}
		if matches {
			directories = append(directories, path)
		}
	}
	return directories, nil
}

func findCodexSessionContext(ctx context.Context, d readerDiscoveryDeps, roots []string, sessionID string) (location Location, err error) {
	defer func() {
		err = readerDiscoveryResult(ctx, err)
		if err != nil {
			location = Location{}
		}
	}()
	if err = readerParseContextErr(ctx); err != nil {
		return Location{}, err
	}
	if !d.complete() {
		return Location{}, conversationlife.ErrRefused
	}
	suffix := "-" + strings.ToLower(sessionID) + ".jsonl"
	if err = readerParseContextErr(ctx); err != nil {
		return Location{}, err
	}
	for _, root := range roots {
		if err = readerParseContextErr(ctx); err != nil {
			return Location{}, err
		}
		years, discoveryErr := descendingDirectoriesContext(ctx, d, root)
		if discoveryErr != nil {
			return Location{}, discoveryErr
		}
		if err = readerParseContextErr(ctx); err != nil {
			return Location{}, err
		}
		for _, year := range years {
			if err = readerParseContextErr(ctx); err != nil {
				return Location{}, err
			}
			yearPath := filepath.Join(root, year)
			if err = readerParseContextErr(ctx); err != nil {
				return Location{}, err
			}
			months, discoveryErr := descendingDirectoriesContext(ctx, d, yearPath)
			if discoveryErr != nil {
				return Location{}, discoveryErr
			}
			if err = readerParseContextErr(ctx); err != nil {
				return Location{}, err
			}
			for _, month := range months {
				if err = readerParseContextErr(ctx); err != nil {
					return Location{}, err
				}
				monthPath := filepath.Join(yearPath, month)
				if err = readerParseContextErr(ctx); err != nil {
					return Location{}, err
				}
				days, discoveryErr := descendingDirectoriesContext(ctx, d, monthPath)
				if discoveryErr != nil {
					return Location{}, discoveryErr
				}
				if err = readerParseContextErr(ctx); err != nil {
					return Location{}, err
				}
				for _, day := range days {
					if err = readerParseContextErr(ctx); err != nil {
						return Location{}, err
					}
					dayPath := filepath.Join(monthPath, day)
					if err = readerParseContextErr(ctx); err != nil {
						return Location{}, err
					}
					var files []os.DirEntry
					readErr := sourceEffect(ctx, func() (effectErr error) { files, effectErr = d.readDir(dayPath); return })
					if terminalProviderError(readErr) {
						return Location{}, readErr
					}
					if readErr != nil {
						continue
					}
					for _, file := range files {
						if err = readerParseContextErr(ctx); err != nil {
							return Location{}, err
						}
						name := strings.ToLower(file.Name())
						if err = readerParseContextErr(ctx); err != nil {
							return Location{}, err
						}
						filePath := filepath.Join(dayPath, file.Name())
						if err = readerParseContextErr(ctx); err != nil {
							return Location{}, err
						}
						directory, discoveryErr := isDirContext(ctx, d, filePath)
						if discoveryErr != nil {
							return Location{}, discoveryErr
						}
						if err = readerParseContextErr(ctx); err != nil {
							return Location{}, err
						}
						if directory {
							continue
						}
						matches := strings.HasPrefix(name, "rollout-") && strings.HasSuffix(name, suffix)
						if err = readerParseContextErr(ctx); err != nil {
							return Location{}, err
						}
						if !matches {
							continue
						}
						path, discoveryErr := containedRegularFileContext(ctx, d, filePath, root)
						if discoveryErr != nil {
							return Location{}, discoveryErr
						}
						if err = readerParseContextErr(ctx); err != nil {
							return Location{}, err
						}
						if path != "" {
							return Location{Path: path, Root: root}, nil
						}
					}
				}
			}
		}
	}
	return Location{}, nil
}

func descendingDirectoriesContext(ctx context.Context, d readerDiscoveryDeps, path string) (directories []string, err error) {
	defer func() {
		err = readerDiscoveryResult(ctx, err)
		if err != nil {
			directories = nil
		}
	}()
	if err = readerParseContextErr(ctx); err != nil {
		return nil, err
	}
	if !d.complete() {
		return nil, conversationlife.ErrRefused
	}
	var entries []os.DirEntry
	err = sourceEffect(ctx, func() (effectErr error) { entries, effectErr = d.readDir(path); return })
	if terminalProviderError(err) {
		return nil, err
	}
	if err != nil {
		return nil, nil
	}
	directories = make([]string, 0, len(entries))
	for _, entry := range entries {
		if err = readerParseContextErr(ctx); err != nil {
			return nil, err
		}
		candidate := filepath.Join(path, entry.Name())
		if err = readerParseContextErr(ctx); err != nil {
			return nil, err
		}
		directory, discoveryErr := isDirContext(ctx, d, candidate)
		if discoveryErr != nil {
			return nil, discoveryErr
		}
		if err = readerParseContextErr(ctx); err != nil {
			return nil, err
		}
		if directory {
			directories = append(directories, entry.Name())
			if err = readerParseContextErr(ctx); err != nil {
				return nil, err
			}
		}
	}
	if err = readerParseContextErr(ctx); err != nil {
		return nil, err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(directories)))
	return directories, nil
}

func resolvePathOrSessionContext(ctx context.Context, d readerDiscoveryDeps, roots []string, sessionID, separator string) (location Location, err error) {
	defer func() {
		err = readerDiscoveryResult(ctx, err)
		if err != nil {
			location = Location{}
		}
	}()
	if err = readerParseContextErr(ctx); err != nil {
		return Location{}, err
	}
	if !d.complete() {
		return Location{}, conversationlife.ErrRefused
	}
	absoluteJSONL := filepath.IsAbs(sessionID) && strings.HasSuffix(strings.ToLower(sessionID), ".jsonl")
	if err = readerParseContextErr(ctx); err != nil {
		return Location{}, err
	}
	if absoluteJSONL {
		for _, root := range roots {
			if err = readerParseContextErr(ctx); err != nil {
				return Location{}, err
			}
			path, discoveryErr := containedRegularFileContext(ctx, d, sessionID, root)
			if discoveryErr != nil {
				return Location{}, discoveryErr
			}
			if err = readerParseContextErr(ctx); err != nil {
				return Location{}, err
			}
			if path != "" {
				return Location{Path: path, Root: root}, nil
			}
		}
		return Location{}, nil
	}
	canonical := canonicalSessionID.MatchString(sessionID)
	if err = readerParseContextErr(ctx); err != nil {
		return Location{}, err
	}
	if !canonical {
		return Location{}, nil
	}
	suffix := separator + strings.ToLower(sessionID) + ".jsonl"
	if err = readerParseContextErr(ctx); err != nil {
		return Location{}, err
	}
	for _, root := range roots {
		if err = readerParseContextErr(ctx); err != nil {
			return Location{}, err
		}
		var directories []os.DirEntry
		readErr := sourceEffect(ctx, func() (effectErr error) { directories, effectErr = d.readDir(root); return })
		if terminalProviderError(readErr) {
			return Location{}, readErr
		}
		if readErr != nil {
			continue
		}
		for _, directory := range directories {
			if err = readerParseContextErr(ctx); err != nil {
				return Location{}, err
			}
			projectDir := filepath.Join(root, directory.Name())
			if err = readerParseContextErr(ctx); err != nil {
				return Location{}, err
			}
			isDirectory, discoveryErr := isDirContext(ctx, d, projectDir)
			if discoveryErr != nil {
				return Location{}, discoveryErr
			}
			if err = readerParseContextErr(ctx); err != nil {
				return Location{}, err
			}
			if !isDirectory {
				continue
			}
			var files []os.DirEntry
			readErr := sourceEffect(ctx, func() (effectErr error) { files, effectErr = d.readDir(projectDir); return })
			if terminalProviderError(readErr) {
				return Location{}, readErr
			}
			if readErr != nil {
				continue
			}
			for _, file := range files {
				if err = readerParseContextErr(ctx); err != nil {
					return Location{}, err
				}
				filePath := filepath.Join(projectDir, file.Name())
				if err = readerParseContextErr(ctx); err != nil {
					return Location{}, err
				}
				isDirectory, discoveryErr := isDirContext(ctx, d, filePath)
				if discoveryErr != nil {
					return Location{}, discoveryErr
				}
				if err = readerParseContextErr(ctx); err != nil {
					return Location{}, err
				}
				if isDirectory {
					continue
				}
				matches := strings.HasSuffix(strings.ToLower(file.Name()), suffix)
				if err = readerParseContextErr(ctx); err != nil {
					return Location{}, err
				}
				if !matches {
					continue
				}
				path, discoveryErr := containedRegularFileContext(ctx, d, filePath, root)
				if discoveryErr != nil {
					return Location{}, discoveryErr
				}
				if err = readerParseContextErr(ctx); err != nil {
					return Location{}, err
				}
				if path != "" {
					return Location{Path: path, Root: root}, nil
				}
			}
		}
	}
	return Location{}, nil
}

func containedRegularFileContext(ctx context.Context, d readerDiscoveryDeps, path, root string) (contained string, err error) {
	defer func() {
		err = readerDiscoveryResult(ctx, err)
		if err != nil {
			contained = ""
		}
	}()
	if err = readerParseContextErr(ctx); err != nil {
		return "", err
	}
	if !d.complete() {
		return "", conversationlife.ErrRefused
	}
	contained, err = (installedSourceDeps{eval: d.eval, stat: d.stat}).contained(ctx, path, root)
	if terminalProviderError(err) {
		return "", err
	}
	if err != nil {
		return "", nil
	}
	return contained, nil
}
