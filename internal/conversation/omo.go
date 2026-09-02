package conversation

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
)

const (
	maxOMOTodoBytes     = 512 * 1024
	maxOMOIdentityBytes = 1024 * 1024
	maxOMOPhases        = 128
	maxOMOTasks         = 1000
	maxOMOTodoString    = 4096
)

var (
	omoFilename = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2}-\d{3}Z_([A-Za-z0-9_-]+)\.jsonl$`)
	omoCWDRun   = regexp.MustCompile(`[/\\:]+`)
)

type OMOTodoTask struct {
	ID      string `json:"id,omitempty"`
	Content string `json:"content"`
	Status  string `json:"status"`
}

type OMOTodoPhase struct {
	Name  string        `json:"name"`
	Tasks []OMOTodoTask `json:"tasks"`
}

type OMOTodoState struct {
	Available  bool           `json:"available"`
	ReasonCode string         `json:"reason_code,omitempty"`
	SessionID  string         `json:"session_id,omitempty"`
	Version    int            `json:"version,omitempty"`
	UpdatedAt  string         `json:"updated_at,omitempty"`
	Phases     []OMOTodoPhase `json:"phases"`
	Truncated  bool           `json:"truncated"`
}

type omoIdentity struct {
	ID  string
	CWD string
}

type omoCacheEntry struct {
	size    int64
	modTime int64
	entries []Entry
	plan    OMOTodoState
	clipped bool
}

func (r *Reader) omoRoots() []string { return agentroots.OMO(r.home) }

func (r *Reader) readOMO(cwd, sessionID, before string, limit int) (Page, error) {
	location, identity, code := r.locateOMO(cwd, strings.TrimSpace(sessionID))
	if code != "" {
		return unavailableCode(code, "OMO conversation history is unavailable."), nil
	}
	entries, plan, clipped, code := r.loadOMO(location.Path, identity, cwd)
	if code != "" {
		reason := "OMO conversation history is unavailable."
		if code == "invalid_session" {
			reason = "OMO session identity does not match the requested session."
		}
		return unavailableCode(code, reason), nil
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
		HasMore: start > 0, Total: len(entries), FileTruncated: clipped, OMOPlan: &plan,
	}, nil
}

// ReadOMOTodoState returns the latest valid native senpi.todo-state row for one
// exact OMO session. Invalid later rows cannot erase an earlier valid v2 state.
func (r *Reader) ReadOMOTodoState(cwd, sessionID string) OMOTodoState {
	location, identity, code := r.locateOMO(cwd, strings.TrimSpace(sessionID))
	if code != "" {
		return OMOTodoState{ReasonCode: code, Phases: []OMOTodoPhase{}}
	}
	_, plan, _, code := r.loadOMO(location.Path, identity, cwd)
	if code != "" {
		return OMOTodoState{ReasonCode: code, Phases: []OMOTodoPhase{}}
	}
	return plan
}

func (r *Reader) loadOMO(path string, identity omoIdentity, cwd string) ([]Entry, OMOTodoState, bool, string) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, OMOTodoState{}, false, "source_unavailable"
	}
	key := path + "\x00" + identity.ID + "\x00" + cwd
	r.mu.Lock()
	cached, ok := r.omoCache[key]
	r.mu.Unlock()
	if ok && cached.size == info.Size() && cached.modTime == info.ModTime().UnixNano() {
		return append([]Entry(nil), cached.entries...), cached.plan, cached.clipped, ""
	}
	if !verifyOMOIdentity(path, identity, cwd) {
		return nil, OMOTodoState{}, false, "invalid_session"
	}
	text, clipped, err := loadTail(path, maxConversationBytes)
	if err != nil {
		return nil, OMOTodoState{}, false, "source_unavailable"
	}
	entries := parseTranscript("pi", text)
	plan := OMOTodoState{
		Available: true, SessionID: identity.ID, Phases: []OMOTodoPhase{}, Truncated: clipped,
	}
	state, found, invalidOnly := latestValidOMOTodo(text, identity.ID)
	if invalidOnly {
		return nil, OMOTodoState{}, false, "source_corrupt"
	}
	if found {
		state.Available = true
		state.Truncated = state.Truncated || clipped
		plan = state
	}
	current, statErr := os.Stat(path)
	if statErr == nil && current.Size() == info.Size() && current.ModTime() == info.ModTime() {
		entry := omoCacheEntry{
			size: info.Size(), modTime: info.ModTime().UnixNano(),
			entries: append([]Entry(nil), entries...), plan: plan, clipped: clipped,
		}
		r.mu.Lock()
		if len(r.omoCache) >= maxOMOCacheEntries {
			clear(r.omoCache)
		}
		r.omoCache[key] = entry
		r.mu.Unlock()
	}
	return entries, plan, clipped, ""
}

func (r *Reader) locateOMO(cwd, sessionID string) (Location, omoIdentity, string) {
	if sessionID == "" {
		return Location{}, omoIdentity{}, "invalid_session"
	}
	if filepath.IsAbs(sessionID) {
		for _, root := range r.omoRoots() {
			path := containedRegularFile(sessionID, root)
			if path == "" {
				continue
			}
			identity, ok := omoIdentityFromPath(path)
			if !ok || !omoPathMatchesCWD(path, cwd) {
				return Location{}, omoIdentity{}, "invalid_session"
			}
			return Location{Path: path, Root: root}, identity, ""
		}
		return Location{}, omoIdentity{}, "path_uncontained"
	}
	if !safeOMOIdentity(sessionID) {
		return Location{}, omoIdentity{}, "invalid_session"
	}
	var match Location
	for _, root := range r.omoRoots() {
		directories := omoProjectDirectories(root, cwd)
		for _, directory := range directories {
			entries, err := os.ReadDir(directory)
			if err != nil {
				continue
			}
			for _, entry := range entries {
				parts := omoFilename.FindStringSubmatch(entry.Name())
				if len(parts) != 2 || parts[1] != sessionID {
					continue
				}
				path := containedRegularFile(filepath.Join(directory, entry.Name()), root)
				if path == "" {
					continue
				}
				if match.Path != "" && match.Path != path {
					return Location{}, omoIdentity{}, "invalid_session"
				}
				match = Location{Path: path, Root: root}
			}
		}
	}
	if match.Path == "" {
		return Location{}, omoIdentity{}, "invalid_session"
	}
	return match, omoIdentity{ID: sessionID, CWD: cwd}, ""
}

func omoProjectDirectories(root, cwd string) []string {
	if strings.TrimSpace(cwd) != "" {
		candidate := filepath.Join(root, encodeOMOCWD(cwd))
		if isDir(candidate) {
			return []string{candidate}
		}
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	directories := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "--") || !strings.HasSuffix(entry.Name(), "--") {
			continue
		}
		path := filepath.Join(root, entry.Name())
		if isDir(path) {
			directories = append(directories, path)
		}
	}
	return directories
}

func encodeOMOCWD(cwd string) string {
	cwd = strings.TrimLeft(strings.TrimSpace(cwd), "/\\")
	return "--" + omoCWDRun.ReplaceAllString(cwd, "-") + "--"
}

func omoPathMatchesCWD(path, cwd string) bool {
	return strings.TrimSpace(cwd) == "" || filepath.Base(filepath.Dir(path)) == encodeOMOCWD(cwd)
}

func omoIdentityFromPath(path string) (omoIdentity, bool) {
	parts := omoFilename.FindStringSubmatch(filepath.Base(path))
	if len(parts) != 2 || !safeOMOIdentity(parts[1]) {
		return omoIdentity{}, false
	}
	return omoIdentity{ID: parts[1]}, true
}

func safeOMOIdentity(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, character := range value {
		switch {
		case character >= 'a' && character <= 'z', character >= 'A' && character <= 'Z', character >= '0' && character <= '9':
		case character == '-', character == '_':
		default:
			return false
		}
	}
	return true
}

func verifyOMOIdentity(path string, expected omoIdentity, requestedCWD string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	scanner := bufio.NewScanner(io.LimitReader(file, maxOMOIdentityBytes))
	scanner.Buffer(make([]byte, 0, 64*1024), maxEntryBytes)
	for scanner.Scan() {
		var record struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			CWD  string `json:"cwd"`
		}
		if json.Unmarshal(scanner.Bytes(), &record) != nil || record.Type != "session" {
			continue
		}
		if record.ID != expected.ID {
			return false
		}
		if strings.TrimSpace(requestedCWD) != "" && record.CWD != requestedCWD {
			return false
		}
		return true
	}
	return false
}

func latestValidOMOTodo(text, sessionID string) (OMOTodoState, bool, bool) {
	var latest OMOTodoState
	found := false
	sawTodo := false
	for len(text) > 0 {
		line := text
		if newline := strings.IndexByte(text, '\n'); newline >= 0 {
			line = text[:newline]
			text = text[newline+1:]
		} else {
			text = ""
		}
		if len(line) > maxOMOTodoBytes {
			continue
		}
		var record struct {
			Type       string          `json:"type"`
			CustomType string          `json:"customType"`
			Timestamp  string          `json:"timestamp"`
			Data       json.RawMessage `json:"data"`
		}
		if json.Unmarshal([]byte(line), &record) != nil || record.Type != "custom" || record.CustomType != "senpi.todo-state" {
			continue
		}
		sawTodo = true
		state, ok := decodeOMOTodo(record.Data, sessionID, record.Timestamp)
		if ok {
			latest = state
			found = true
		}
	}
	return latest, found, sawTodo && !found
}

func decodeOMOTodo(data json.RawMessage, sessionID, timestamp string) (OMOTodoState, bool) {
	var native struct {
		Schema string `json:"schema"`
		Phases []struct {
			Name  string `json:"name"`
			Tasks []struct {
				ID      string `json:"id"`
				Content string `json:"content"`
				Status  string `json:"status"`
			} `json:"tasks"`
		} `json:"phases"`
	}
	if json.Unmarshal(data, &native) != nil || native.Schema != "v2" || native.Phases == nil {
		return OMOTodoState{}, false
	}
	state := OMOTodoState{SessionID: sessionID, Version: 2, UpdatedAt: timestamp, Phases: []OMOTodoPhase{}}
	taskCount := 0
	for phaseIndex, phase := range native.Phases {
		name := strings.TrimSpace(phase.Name)
		if name == "" || len(name) > maxOMOTodoString || phase.Tasks == nil {
			return OMOTodoState{}, false
		}
		if phaseIndex >= maxOMOPhases {
			state.Truncated = true
			continue
		}
		outputPhase := OMOTodoPhase{Name: name, Tasks: []OMOTodoTask{}}
		for _, task := range phase.Tasks {
			content := strings.TrimSpace(task.Content)
			if content == "" || len(content) > maxOMOTodoString || !validOMOTodoStatus(task.Status) || len(task.ID) > 128 {
				return OMOTodoState{}, false
			}
			if taskCount >= maxOMOTasks {
				state.Truncated = true
				continue
			}
			outputPhase.Tasks = append(outputPhase.Tasks, OMOTodoTask{ID: task.ID, Content: content, Status: task.Status})
			taskCount++
		}
		state.Phases = append(state.Phases, outputPhase)
	}
	return state, true
}

func validOMOTodoStatus(status string) bool {
	switch status {
	case "pending", "in_progress", "completed", "abandoned":
		return true
	default:
		return false
	}
}

func parseOMOTimestamp(name string) (time.Time, bool) {
	parts := omoFilename.FindStringSubmatch(name)
	if len(parts) != 2 {
		return time.Time{}, false
	}
	value := strings.SplitN(strings.TrimSuffix(name, ".jsonl"), "_", 2)[0]
	parsed, err := time.Parse("2006-01-02T15-04-05-000Z", value)
	return parsed, err == nil
}
