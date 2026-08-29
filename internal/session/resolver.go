package session

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
	"github.com/0cv/herdr-mobile-relay/internal/conversation"
)

const cacheTTL = 60 * time.Second

type cacheEntry struct {
	name    string
	expires time.Time
	sig     string
}

type Resolver struct {
	mu     sync.Mutex
	cache  map[string]cacheEntry
	home   string
	reader *conversation.Reader
}

func NewResolver(home string) *Resolver {
	return &Resolver{
		cache:  make(map[string]cacheEntry),
		home:   home,
		reader: conversation.NewReader(home),
	}
}

// Root accessors remain useful in tests that pin configuration precedence.
func (r *Resolver) claudeRoots() []string { return agentroots.Claude(r.home) }
func (r *Resolver) qoderRoots() []string  { return agentroots.Qoder(r.home) }
func (r *Resolver) codexHomes() []string  { return agentroots.CodexHomes(r.home) }
func (r *Resolver) piRoots() []string     { return agentroots.Pi(r.home) }
func (r *Resolver) ompRoots() []string    { return agentroots.OMP(r.home) }

func (r *Resolver) SessionName(agent, cwd, sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return ""
	}

	// Title resolution starts from the exact contained transcript selected by
	// conversation.Reader. The title and conversation therefore cannot diverge
	// on duplicate IDs, configured-root precedence, or symlink containment.
	location := r.reader.Locate(agent, cwd, sessionID)
	if location.Path == "" {
		return ""
	}

	agentLower := strings.ToLower(strings.TrimSpace(agent))
	key := agentLower + "|" + cwd + "|" + sessionID
	sig := locationSignature(agentLower, location)

	r.mu.Lock()
	if entry, ok := r.cache[key]; ok && entry.sig == sig && time.Now().Before(entry.expires) {
		r.mu.Unlock()
		return entry.name
	}
	r.mu.Unlock()

	var name string
	switch {
	case isOMPSessionAgent(agentLower):
		name = extractOMPSessionTitle(location.Path)
	case isPiSessionAgent(agentLower):
		name = extractPiSessionTitle(location.Path)
	case strings.Contains(agentLower, "qoder"), strings.Contains(agentLower, "claude"):
		name = extractTitle(location.Path)
	case strings.Contains(agentLower, "codex"):
		name = codexIndexThreadName(filepath.Join(filepath.Dir(location.Root), "session_index.jsonl"), sessionID)
	}

	r.mu.Lock()
	r.cache[key] = cacheEntry{name: name, expires: time.Now().Add(cacheTTL), sig: sig}
	r.mu.Unlock()
	return name
}

func isOMPSessionAgent(agent string) bool {
	switch strings.ToLower(strings.TrimSpace(agent)) {
	case "omp", "oh-my-pi", "oh my pi", "ohmypi":
		return true
	default:
		return false
	}
}

func isPiSessionAgent(agent string) bool {
	switch strings.ToLower(strings.TrimSpace(agent)) {
	case "pi", "pi-coding-agent":
		return true
	default:
		return false
	}
}

func extractOMPSessionTitle(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	var sessionTitle string
	var headerTitle string
	hasHeaderTitle := false
	var latestTitle string
	hasTitleEvent := false
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)
	for scanner.Scan() {
		var record struct {
			Type  string `json:"type"`
			Title string `json:"title"`
		}
		if json.Unmarshal(scanner.Bytes(), &record) != nil {
			continue
		}
		switch record.Type {
		case "title":
			if !hasHeaderTitle {
				hasHeaderTitle = true
				headerTitle = strings.TrimSpace(record.Title)
			}
		case "session":
			sessionTitle = strings.TrimSpace(record.Title)
		case "title_change":
			hasTitleEvent = true
			latestTitle = strings.TrimSpace(record.Title)
		}
	}
	if hasHeaderTitle {
		return headerTitle
	}
	if hasTitleEvent {
		return latestTitle
	}
	return sessionTitle
}

func extractPiSessionTitle(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	var name string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)
	for scanner.Scan() {
		var record struct {
			Type string `json:"type"`
			Name string `json:"name"`
		}
		if json.Unmarshal(scanner.Bytes(), &record) != nil {
			continue
		}
		if record.Type == "session_info" {
			name = strings.TrimSpace(record.Name)
		}
	}
	return name
}

func codexIndexThreadName(indexFile, sessionID string) string {
	f, err := os.Open(indexFile)
	if err != nil {
		return ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var record struct {
			ID         string `json:"id"`
			ThreadName string `json:"thread_name"`
		}
		if json.Unmarshal(scanner.Bytes(), &record) == nil && record.ID == sessionID {
			return record.ThreadName
		}
	}
	return ""
}

var titleFields = []string{"customTitle", "aiTitle", "title", "summary", "text", "name", "value"}
var titleTypes = map[string]bool{"custom-title": true, "ai-title": true, "summary": true}

func extractTitle(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	found := make(map[string]string)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)
	for scanner.Scan() {
		var record map[string]any
		if json.Unmarshal(scanner.Bytes(), &record) != nil {
			continue
		}
		recordType, _ := record["type"].(string)
		if !titleTypes[recordType] {
			continue
		}
		for _, field := range titleFields {
			if value, ok := record[field].(string); ok && strings.TrimSpace(value) != "" {
				found[recordType] = strings.TrimSpace(value)
				break
			}
		}
	}
	for _, recordType := range []string{"custom-title", "ai-title", "summary"} {
		if found[recordType] != "" {
			return found[recordType]
		}
	}
	return ""
}

func locationSignature(agent string, location conversation.Location) string {
	signature := pathSignature(location.Path)
	if strings.Contains(agent, "codex") {
		signature += "|" + pathSignature(filepath.Join(filepath.Dir(location.Root), "session_index.jsonl"))
	}
	return signature
}

func pathSignature(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return path + "|missing"
	}
	return path + "|" + strconv.FormatInt(info.ModTime().UnixNano(), 10) + "|" + strconv.FormatInt(info.Size(), 10)
}
