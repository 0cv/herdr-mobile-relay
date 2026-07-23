package session

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const cacheTTL = 60 * time.Second

type cacheEntry struct {
	name    string
	expires time.Time
	sig     string
}

type Resolver struct {
	mu    sync.Mutex
	cache map[string]cacheEntry
	home  string
}

func NewResolver(home string) *Resolver {
	return &Resolver{
		cache: make(map[string]cacheEntry),
		home:  home,
	}
}

func (r *Resolver) SessionName(agent, cwd, sessionID string) string {
	if sessionID == "" {
		return ""
	}
	if !validSessionID(sessionID) {
		return ""
	}

	key := agent + "|" + cwd + "|" + sessionID

	r.mu.Lock()
	if entry, ok := r.cache[key]; ok && time.Now().Before(entry.expires) {
		r.mu.Unlock()
		return entry.name
	}
	r.mu.Unlock()

	var name string
	agentLower := strings.ToLower(agent)
	switch {
	case strings.Contains(agentLower, "qoder"):
		name = r.projectSessionTitle(filepath.Join(r.home, ".qoder", "projects"), cwd, sessionID)
	case strings.Contains(agentLower, "claude"):
		name = r.projectSessionTitle(filepath.Join(r.home, ".claude", "projects"), cwd, sessionID)
	case strings.Contains(agentLower, "codex"):
		name = r.codexSessionName(cwd, sessionID)
	}

	r.mu.Lock()
	r.cache[key] = cacheEntry{name: name, expires: time.Now().Add(cacheTTL)}
	r.mu.Unlock()

	return name
}

func (r *Resolver) projectSessionTitle(projectsDir, cwd, sessionID string) string {
	projectDir := r.findProjectDir(projectsDir, cwd)
	if projectDir == "" {
		return ""
	}

	sessionFile := filepath.Join(projectDir, sessionID+".jsonl")
	if _, err := os.Stat(sessionFile); err != nil {
		entries, err := os.ReadDir(projectDir)
		if err != nil {
			return ""
		}
		var jsonlFiles []string
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
				jsonlFiles = append(jsonlFiles, e.Name())
			}
		}
		if len(jsonlFiles) == 1 {
			sessionFile = filepath.Join(projectDir, jsonlFiles[0])
		} else {
			return ""
		}
	}

	return extractTitle(sessionFile)
}

func (r *Resolver) findProjectDir(projectsDir, cwd string) string {
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return ""
	}

	encoded := encodePath(cwd)
	for _, e := range entries {
		if e.IsDir() && e.Name() == encoded {
			return filepath.Join(projectsDir, e.Name())
		}
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(projectsDir, e.Name())
		if matchesCwd(dir, cwd) {
			return dir
		}
	}
	return ""
}

func (r *Resolver) codexSessionName(cwd, sessionID string) string {
	indexFile := filepath.Join(r.home, ".codex", "session_index.jsonl")
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

	var lastTitle string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)
	for scanner.Scan() {
		var record map[string]any
		if json.Unmarshal(scanner.Bytes(), &record) != nil {
			continue
		}
		recType, _ := record["type"].(string)
		if !titleTypes[recType] {
			continue
		}
		for _, field := range titleFields {
			if v, ok := record[field].(string); ok && v != "" {
				lastTitle = v
				break
			}
		}
	}
	return lastTitle
}

func encodePath(path string) string {
	return strings.ReplaceAll(strings.TrimPrefix(path, "/"), "/", "-")
}

func matchesCwd(dir, cwd string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "cwd"))
	if err == nil {
		return strings.TrimSpace(string(data)) == cwd
	}
	return false
}

func validSessionID(id string) bool {
	if len(id) > 256 {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return !strings.Contains(id, "..")
}
