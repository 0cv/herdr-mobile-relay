package session

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
	sessionID = strings.TrimSpace(sessionID)
	agentLower := strings.ToLower(agent)
	sessionPath := ""
	if isOMPSessionAgent(agentLower) {
		sessionPath = r.ompSessionPath(sessionID)
		if sessionPath == "" {
			return ""
		}
	} else if isPiSessionAgent(agentLower) {
		sessionPath = r.piSessionPath(sessionID)
		if sessionPath == "" {
			return ""
		}
	} else if sessionID != "" && !validSessionID(sessionID) {
		return ""
	}

	key := agent + "|" + cwd + "|" + sessionID
	sig := r.sourceSignature(agent, cwd, sessionID)

	r.mu.Lock()
	if entry, ok := r.cache[key]; ok && entry.sig == sig && time.Now().Before(entry.expires) {
		r.mu.Unlock()
		return entry.name
	}
	r.mu.Unlock()

	var name string
	switch {
	case isOMPSessionAgent(agentLower):
		name = extractOMPSessionTitle(sessionPath)
	case isPiSessionAgent(agentLower):
		name = extractPiSessionTitle(sessionPath)
	case strings.Contains(agentLower, "qoder"):
		name = r.projectSessionTitle(filepath.Join(r.home, ".qoder", "projects"), cwd, sessionID)
	case strings.Contains(agentLower, "claude"):
		name = r.projectSessionTitle(filepath.Join(r.home, ".claude", "projects"), cwd, sessionID)
	case strings.Contains(agentLower, "codex"):
		name = r.codexSessionName(cwd, sessionID)
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
	return strings.EqualFold(strings.TrimSpace(agent), "pi")
}

func (r *Resolver) ompSessionPath(sessionID string) string {
	if !filepath.IsAbs(sessionID) || filepath.Ext(sessionID) != ".jsonl" {
		return ""
	}
	root, err := filepath.Abs(filepath.Join(r.home, ".omp", "agent", "sessions"))
	if err != nil {
		return ""
	}
	path, err := filepath.Abs(filepath.Clean(sessionID))
	if err != nil {
		return ""
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return ""
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return ""
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	return path
}

func extractOMPSessionTitle(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	var sessionTitle string
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
		case "session":
			sessionTitle = strings.TrimSpace(record.Title)
		case "title", "title_change":
			hasTitleEvent = true
			latestTitle = strings.TrimSpace(record.Title)
		}
	}
	if hasTitleEvent {
		return latestTitle
	}
	return sessionTitle
}

func (r *Resolver) piSessionPath(sessionID string) string {
	if !filepath.IsAbs(sessionID) || filepath.Ext(sessionID) != ".jsonl" {
		return ""
	}
	root, err := filepath.Abs(filepath.Join(r.home, ".pi", "agent", "sessions"))
	if err != nil {
		return ""
	}
	path, err := filepath.Abs(filepath.Clean(sessionID))
	if err != nil {
		return ""
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return ""
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return ""
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	return path
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

func (r *Resolver) projectSessionTitle(projectsDir, cwd, sessionID string) string {
	projectDir := r.findProjectDir(projectsDir, cwd)
	if projectDir == "" {
		return ""
	}

	sessionFile := filepath.Join(projectDir, sessionID+".jsonl")
	if _, err := os.Stat(sessionFile); err != nil {
		if sessionID != "" {
			return ""
		}
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
	leadingDashEncoded := "-" + encoded
	for _, e := range entries {
		if e.IsDir() && (e.Name() == encoded || e.Name() == leadingDashEncoded) {
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

	found := make(map[string]string)
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
			if v, ok := record[field].(string); ok && strings.TrimSpace(v) != "" {
				found[recType] = strings.TrimSpace(v)
				break
			}
		}
	}
	for _, recType := range []string{"custom-title", "ai-title", "summary"} {
		if found[recType] != "" {
			return found[recType]
		}
	}
	return ""
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
	if len(id) == 0 || len(id) > 128 {
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
	return true
}

func (r *Resolver) sourceSignature(agent, cwd, sessionID string) string {
	agentLower := strings.ToLower(agent)
	if isOMPSessionAgent(agentLower) {
		if path := r.ompSessionPath(sessionID); path != "" {
			return pathSignature(path)
		}
		return ""
	}
	if isPiSessionAgent(agentLower) {
		if path := r.piSessionPath(sessionID); path != "" {
			return pathSignature(path)
		}
		return ""
	}

	if strings.Contains(agentLower, "codex") {
		return pathSignature(filepath.Join(r.home, ".codex", "session_index.jsonl"))
	}
	var projectsDir string
	switch {
	case strings.Contains(agentLower, "qoder"):
		projectsDir = filepath.Join(r.home, ".qoder", "projects")
	case strings.Contains(agentLower, "claude"):
		projectsDir = filepath.Join(r.home, ".claude", "projects")
	default:
		return ""
	}
	projectDir := r.findProjectDir(projectsDir, cwd)
	if projectDir == "" {
		return pathSignature(projectsDir)
	}
	if sessionID != "" {
		return pathSignature(filepath.Join(projectDir, sessionID+".jsonl"))
	}
	entries, _ := os.ReadDir(projectDir)
	var signatures []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".jsonl") {
			signatures = append(signatures, pathSignature(filepath.Join(projectDir, entry.Name())))
		}
	}
	sort.Strings(signatures)
	return strings.Join(signatures, "|")
}

func pathSignature(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return path + "|missing"
	}
	return path + "|" + strconv.FormatInt(info.ModTime().UnixNano(), 10) + "|" + strconv.FormatInt(info.Size(), 10)
}
