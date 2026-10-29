package conversation

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
	"github.com/0cv/herdr-mobile-relay/internal/conversationlife"
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
	size     int64
	modTime  int64
	identity string
	entries  []Entry
	plan     OMOTodoState
	clipped  bool
}

func (r *Reader) omoRoots() []string { return agentroots.OMO(r.home) }

func (r *Reader) readOMO(cwd, sessionID, before string, limit int) (Page, error) {
	location, identity, code := r.locateOMO(cwd, strings.TrimSpace(sessionID))
	if code != "" {
		return unavailableCode(code, "OMO conversation history is unavailable."), nil
	}
	entries, plan, clipped, code := r.loadOMO(location, identity, cwd)
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
		HasMore: start > 0, Total: len(entries), FileTruncated: clipped, SourceCorrupt: plan.ReasonCode == "source_corrupt", OMOPlan: &plan,
	}, nil
}

// ReadOMOTodoState returns the latest valid native senpi.todo-state row for one
// exact OMO session. Invalid later rows cannot erase an earlier valid v2 state.
func (r *Reader) ReadOMOTodoState(cwd, sessionID string) OMOTodoState {
	if r != nil && r.installed != nil {
		state, _ := r.ReadOMOTodoStateContext(context.Background(), cwd, sessionID)
		return state
	}
	location, identity, code := r.locateOMO(cwd, strings.TrimSpace(sessionID))
	if code != "" {
		return OMOTodoState{ReasonCode: code, Phases: []OMOTodoPhase{}}
	}
	_, plan, _, code := r.loadOMO(location, identity, cwd)
	if code != "" {
		return OMOTodoState{ReasonCode: code, Phases: []OMOTodoPhase{}}
	}
	return plan
}

func (r *Reader) loadOMO(location Location, identity omoIdentity, cwd string) ([]Entry, OMOTodoState, bool, string) {
	source, err := captureFileSource(location)
	if err != nil {
		return nil, OMOTodoState{}, false, "source_unavailable"
	}
	defer source.close()
	file, ok := source.file.(*os.File)
	if !ok || !verifyOMOIdentityFile(file, identity, cwd) {
		return nil, OMOTodoState{}, false, "invalid_session"
	}
	key := source.path + "\x00" + identity.ID + "\x00" + cwd
	sourceIdentity := fileIdentity(source.info)
	r.mu.Lock()
	cached, ok := r.omoCache[key]
	r.mu.Unlock()
	if ok && cached.size == source.info.Size() && cached.modTime == source.info.ModTime().UnixNano() && cached.identity == sourceIdentity {
		return append([]Entry(nil), cached.entries...), cached.plan, cached.clipped, ""
	}
	text, clipped, err := loadTailFile(file, maxConversationBytes)
	if err != nil {
		return nil, OMOTodoState{}, false, "source_unavailable"
	}
	entries := parseTranscript("pi", text)
	plan := OMOTodoState{
		Available: true, SessionID: identity.ID, Phases: []OMOTodoPhase{}, Truncated: clipped,
	}
	state, found, invalidOnly := latestValidOMOTodo(text, identity.ID)
	if found {
		state.Available = true
		state.Truncated = state.Truncated || clipped
		plan = state
	} else if invalidOnly {
		plan.ReasonCode = "source_corrupt"
	}
	current, statErr := os.Stat(source.path)
	if statErr == nil && os.SameFile(source.info, current) && current.Size() == source.info.Size() && current.ModTime() == source.info.ModTime() {
		entry := omoCacheEntry{
			size: source.info.Size(), modTime: source.info.ModTime().UnixNano(), identity: sourceIdentity,
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
	file, err := openConversationSource(path)
	if err != nil {
		return false
	}
	defer file.Close()
	return verifyOMOIdentityFile(file, expected, requestedCWD)
}

func verifyOMOIdentityFile(file *os.File, expected omoIdentity, requestedCWD string) bool {
	if file == nil {
		return false
	}
	matched, _ := verifyOMOIdentityReaderContext(context.Background(), file, expected, requestedCWD)
	return matched
}

func latestValidOMOTodo(text, sessionID string) (OMOTodoState, bool, bool) {
	latest, found, invalidOnly, _ := latestValidOMOTodoContext(context.Background(), text, sessionID)
	return latest, found, invalidOnly
}

func decodeOMOTodo(data json.RawMessage, sessionID, timestamp string) (OMOTodoState, bool) {
	state, valid, _ := decodeOMOTodoContext(context.Background(), data, sessionID, timestamp)
	return state, valid
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

func verifyOMOIdentityReaderContext(ctx context.Context, reader io.Reader, expected omoIdentity, requestedCWD string) (matched bool, err error) {
	var readErr error
	defer func() {
		// Ordinary scanner errors remain nonsemantic unless cancellation or a
		// terminal identity accompanies them, including at final publication.
		if readErr != nil && (terminalProviderError(readErr) || readerParseContextErr(ctx) != nil) {
			err = readerDiscoveryResult(ctx, readErr)
		}
		err = readerDiscoveryResult(ctx, err)
		if err != nil {
			matched = false
		}
	}()
	if err = readerParseContextErr(ctx); err != nil {
		return false, err
	}
	if reader == nil {
		return false, conversationlife.ErrRefused
	}
	scanner := bufio.NewScanner(io.LimitReader(reader, maxOMOIdentityBytes))
	scanner.Buffer(make([]byte, 0, 64*1024), maxEntryBytes)
	for {
		if err = readerParseContextErr(ctx); err != nil {
			return false, err
		}
		scanned := scanner.Scan()
		// Scanner can return a buffered record alongside a terminal read error.
		// Retain that primary error together with actual cancellation before
		// applying the ordinary scanner-failure/first-session policy.
		readErr = scanner.Err()
		cancelled := readerParseContextErr(ctx)
		if terminalProviderError(readErr) || cancelled != nil {
			err = readerDiscoveryResult(ctx, readErr)
			if err == nil {
				err = cancelled
			}
			return false, err
		}
		if !scanned {
			return false, nil
		}
		var record struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			CWD  string `json:"cwd"`
		}
		decodeErr := json.Unmarshal(scanner.Bytes(), &record)
		if err = readerParseContextErr(ctx); err != nil {
			return false, readerDiscoveryResult(ctx, readErr)
		}
		if decodeErr != nil || record.Type != "session" {
			continue
		}
		if record.ID != expected.ID {
			return false, nil
		}
		cwdRequired := strings.TrimSpace(requestedCWD) != ""
		if err = readerParseContextErr(ctx); err != nil {
			return false, readerDiscoveryResult(ctx, readErr)
		}
		if cwdRequired && record.CWD != requestedCWD {
			return false, nil
		}
		return true, nil
	}
}

func latestValidOMOTodoContext(ctx context.Context, text, sessionID string) (latest OMOTodoState, found bool, invalidOnly bool, err error) {
	defer func() {
		err = readerDiscoveryResult(ctx, err)
		if err != nil {
			latest, found, invalidOnly = OMOTodoState{}, false, false
		}
	}()
	if err = readerParseContextErr(ctx); err != nil {
		return OMOTodoState{}, false, false, err
	}
	sawTodo := false
	for len(text) > 0 {
		if err = readerParseContextErr(ctx); err != nil {
			return OMOTodoState{}, false, false, err
		}
		line := text
		if newline := strings.IndexByte(text, '\n'); newline >= 0 {
			line = text[:newline]
			text = text[newline+1:]
		} else {
			text = ""
		}
		if err = readerParseContextErr(ctx); err != nil {
			return OMOTodoState{}, false, false, err
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
		decodeErr := json.Unmarshal([]byte(line), &record)
		if err = readerParseContextErr(ctx); err != nil {
			return OMOTodoState{}, false, false, err
		}
		if decodeErr != nil || record.Type != "custom" || record.CustomType != "senpi.todo-state" {
			continue
		}
		sawTodo = true
		state, ok, decodeErr := decodeOMOTodoContext(ctx, record.Data, sessionID, record.Timestamp)
		if err = readerDiscoveryResult(ctx, decodeErr); err != nil {
			return OMOTodoState{}, false, false, err
		}
		if ok {
			latest = state
			found = true
		}
	}
	return latest, found, sawTodo && !found, nil
}

func decodeOMOTodoContext(ctx context.Context, data json.RawMessage, sessionID, timestamp string) (state OMOTodoState, valid bool, err error) {
	defer func() {
		err = readerDiscoveryResult(ctx, err)
		if err != nil {
			state, valid = OMOTodoState{}, false
		}
	}()
	if err = readerParseContextErr(ctx); err != nil {
		return OMOTodoState{}, false, err
	}
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
	decodeErr := json.Unmarshal(data, &native)
	if err = readerParseContextErr(ctx); err != nil {
		return OMOTodoState{}, false, err
	}
	if decodeErr != nil || native.Schema != "v2" || native.Phases == nil {
		return OMOTodoState{}, false, nil
	}
	state = OMOTodoState{SessionID: sessionID, Version: 2, UpdatedAt: timestamp, Phases: []OMOTodoPhase{}}
	taskCount := 0
	for phaseIndex, phase := range native.Phases {
		if err = readerParseContextErr(ctx); err != nil {
			return OMOTodoState{}, false, err
		}
		name := strings.TrimSpace(phase.Name)
		if err = readerParseContextErr(ctx); err != nil {
			return OMOTodoState{}, false, err
		}
		if name == "" || len(name) > maxOMOTodoString || phase.Tasks == nil {
			return OMOTodoState{}, false, nil
		}
		if phaseIndex >= maxOMOPhases {
			state.Truncated = true
			continue
		}
		outputPhase := OMOTodoPhase{Name: name, Tasks: []OMOTodoTask{}}
		for _, task := range phase.Tasks {
			if err = readerParseContextErr(ctx); err != nil {
				return OMOTodoState{}, false, err
			}
			content := strings.TrimSpace(task.Content)
			if err = readerParseContextErr(ctx); err != nil {
				return OMOTodoState{}, false, err
			}
			if content == "" || len(content) > maxOMOTodoString || !validOMOTodoStatus(task.Status) || len(task.ID) > 128 {
				return OMOTodoState{}, false, nil
			}
			if taskCount >= maxOMOTasks {
				state.Truncated = true
				continue
			}
			outputPhase.Tasks = append(outputPhase.Tasks, OMOTodoTask{ID: task.ID, Content: content, Status: task.Status})
			taskCount++
		}
		if err = readerParseContextErr(ctx); err != nil {
			return OMOTodoState{}, false, err
		}
		state.Phases = append(state.Phases, outputPhase)
	}
	return state, true, nil
}
