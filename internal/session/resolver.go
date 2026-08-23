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

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
)

const cacheTTL = 60 * time.Second

type cacheEntry struct {
	name    string
	expires time.Time
	sig     string
}

type Resolver struct {
	mu          sync.Mutex
	cache       map[string]cacheEntry
	claudeRoots []string
	qoderRoots  []string
	codexHomes  []string
	piRoots     []string
	ompRoots    []string
}

// NewResolver resolves every agent's roots once, through the same seam
// conversation.NewReader uses, so a pane's title and its transcript can never
// be looked up in different trees.
func NewResolver(home string) *Resolver {
	return &Resolver{
		cache:       make(map[string]cacheEntry),
		claudeRoots: agentroots.Claude(home),
		qoderRoots:  agentroots.Qoder(home),
		codexHomes:  agentroots.CodexHomes(home),
		piRoots:     agentroots.Pi(home),
		ompRoots:    agentroots.OMP(home),
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
		name = r.projectSessionTitle(r.qoderRoots, cwd, sessionID)
	case strings.Contains(agentLower, "claude"):
		name = r.projectSessionTitle(r.claudeRoots, cwd, sessionID)
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
	switch strings.ToLower(strings.TrimSpace(agent)) {
	case "pi", "pi-coding-agent":
		return true
	default:
		return false
	}
}

func (r *Resolver) ompSessionPath(sessionID string) string {
	return containedSessionFile(r.ompRoots, sessionID)
}

// containedSessionFile reports the resolved path of sessionID when it names a
// regular .jsonl file inside at least one root. The predicates are the original
// single-root check - IsAbs, .jsonl, Abs, EvalSymlinks, Rel, Mode().IsRegular()
// - unchanged; the path-side ones are hoisted out of the loop because they do
// not depend on the root, and every root-side bail-out becomes "try the next
// root" so a root that fails to resolve is skipped rather than treated as a
// match. Multi-root means contained in at least one root, never containment
// skipped.
func containedSessionFile(roots []string, sessionID string) string {
	if !filepath.IsAbs(sessionID) || filepath.Ext(sessionID) != ".jsonl" {
		return ""
	}
	path, err := filepath.Abs(filepath.Clean(sessionID))
	if err != nil {
		return ""
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return ""
	}
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return ""
	}
	for _, candidate := range roots {
		root, err := filepath.Abs(candidate)
		if err != nil {
			continue
		}
		root, err = filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		return path
	}
	return ""
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
			// OMP rewrites the first title record in place. Later
			// title_change records are history, not the current name.
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

func (r *Resolver) piSessionPath(sessionID string) string {
	return containedSessionFile(r.piRoots, sessionID)
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

// projectSessionTitle returns the first non-empty title across the agent's
// roots. A root whose project directory exists but does not hold this session
// must not end the search - the session may live in another profile.
func (r *Resolver) projectSessionTitle(roots []string, cwd, sessionID string) string {
	for _, projectsDir := range roots {
		if title := r.projectTitleInRoot(projectsDir, cwd, sessionID); title != "" {
			return title
		}
	}
	return ""
}

func (r *Resolver) projectTitleInRoot(projectsDir, cwd, sessionID string) string {
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
	for _, codexHome := range r.codexHomes {
		if name := codexIndexThreadName(filepath.Join(codexHome, "session_index.jsonl"), sessionID); name != "" {
			return name
		}
	}
	return ""
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
		return rootsSignature(r.codexHomes, "session_index.jsonl")
	}
	var projectsRoots []string
	switch {
	case strings.Contains(agentLower, "qoder"):
		projectsRoots = r.qoderRoots
	case strings.Contains(agentLower, "claude"):
		projectsRoots = r.claudeRoots
	default:
		return ""
	}
	// Sign the candidate in EVERY root, never just the first root that happens
	// to have a project directory for this cwd. projectSessionTitle keeps
	// searching until a root yields a non-empty title, so stopping here at an
	// earlier root would sign a file the title did not come from and pin a
	// stale title for the whole cache TTL. With exactly one root - every
	// default, unconfigured install - this reduces to the original signature.
	signatures := make([]string, 0, len(projectsRoots))
	for _, projectsDir := range projectsRoots {
		projectDir := r.findProjectDir(projectsDir, cwd)
		if projectDir == "" {
			signatures = append(signatures, pathSignature(projectsDir))
			continue
		}
		if sessionID != "" {
			signatures = append(signatures, pathSignature(filepath.Join(projectDir, sessionID+".jsonl")))
			continue
		}
		// With no session id the title falls back to the sole .jsonl in the
		// directory, so the choice depends on the whole listing: a second file
		// appearing must invalidate the cache.
		entries, _ := os.ReadDir(projectDir)
		listing := make([]string, 0, len(entries))
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".jsonl") {
				listing = append(listing, pathSignature(filepath.Join(projectDir, entry.Name())))
			}
		}
		sort.Strings(listing)
		signatures = append(signatures, strings.Join(listing, "|"))
	}
	return strings.Join(signatures, "|")
}

// rootsSignature signs one file per root (or the root itself when leaf is
// empty), joined in root order. The root list is fixed when the Resolver is
// built, so the result is deterministic.
func rootsSignature(roots []string, leaf string) string {
	signatures := make([]string, 0, len(roots))
	for _, root := range roots {
		path := root
		if leaf != "" {
			path = filepath.Join(root, leaf)
		}
		signatures = append(signatures, pathSignature(path))
	}
	return strings.Join(signatures, "|")
}

func pathSignature(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return path + "|missing"
	}
	return path + "|" + strconv.FormatInt(info.ModTime().UnixNano(), 10) + "|" + strconv.FormatInt(info.Size(), 10)
}
