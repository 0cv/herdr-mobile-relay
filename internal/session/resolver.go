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
	mu    sync.Mutex
	cache map[string]cacheEntry
	home  string
}

// NewResolver keeps the home directory and nothing else. Every root is
// resolved per call, through the same agentroots seam conversation.Reader
// uses, so a pane's title and its transcript can never be looked up in
// different trees.
//
// The roots are deliberately not resolved once here. agentroots discovers Pi
// and Oh My Pi named profiles by reading <config root>/profiles, and the relay
// is a long-lived user service: a profile directory appears the first time
// someone runs the agent under that profile, which is almost always after the
// relay started. A snapshot taken at construction would leave every pane in
// that profile unresolvable until the service was restarted.
func NewResolver(home string) *Resolver {
	return &Resolver{cache: make(map[string]cacheEntry), home: home}
}

func (r *Resolver) claudeRoots() []string { return agentroots.Claude(r.home) }
func (r *Resolver) qoderRoots() []string  { return agentroots.Qoder(r.home) }
func (r *Resolver) codexHomes() []string  { return agentroots.CodexHomes(r.home) }
func (r *Resolver) piRoots() []string     { return agentroots.Pi(r.home) }
func (r *Resolver) ompRoots() []string    { return agentroots.OMP(r.home) }

// agentRoots reports the directories to search for one agent, or nil when the
// agent has no title source at all. The precedence matches the switch in
// SessionName: Pi and Oh My Pi are matched by exact name, everything else by
// substring.
func (r *Resolver) agentRoots(agentLower string) []string {
	switch {
	case isOMPSessionAgent(agentLower):
		return r.ompRoots()
	case isPiSessionAgent(agentLower):
		return r.piRoots()
	case strings.Contains(agentLower, "qoder"):
		return r.qoderRoots()
	case strings.Contains(agentLower, "claude"):
		return r.claudeRoots()
	case strings.Contains(agentLower, "codex"):
		return r.codexHomes()
	}
	return nil
}

func (r *Resolver) SessionName(agent, cwd, sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	agentLower := strings.ToLower(agent)

	// Resolved once and threaded through every helper below. Each accessor
	// reads the filesystem, so calling one twice in a single request both pays
	// for the scan twice and risks deciding the title, its cache signature and
	// the containment check against three different lists.
	roots := r.agentRoots(agentLower)

	sessionPath := ""
	if isOMPSessionAgent(agentLower) || isPiSessionAgent(agentLower) {
		// Pi and Oh My Pi panes report the transcript path itself, not an id.
		sessionPath = containedSessionFile(roots, sessionID)
		if sessionPath == "" {
			return ""
		}
	} else if sessionID != "" && !validSessionID(sessionID) {
		return ""
	}

	key := agent + "|" + cwd + "|" + sessionID
	sig := sourceSignature(agentLower, cwd, sessionID, sessionPath, roots)

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
	case strings.Contains(agentLower, "qoder"), strings.Contains(agentLower, "claude"):
		name = projectSessionTitle(roots, cwd, sessionID)
	case strings.Contains(agentLower, "codex"):
		name = codexSessionName(roots, sessionID)
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

// projectSessionTitle returns the title held by the FIRST root that holds this
// session - not the first root that yields a non-empty title.
// conversation.Reader picks the transcript by exactly the same
// first-root-that-holds-the-file rule, and the two have to agree. A freshly
// started session has no title record yet, so stopping on the first non-empty
// title would let a titleless copy in an earlier root hand the pane a later
// root's title while the reader shows the earlier root's transcript: a
// correct-looking header over someone else's conversation, which is the split
// this seam exists to prevent.
func projectSessionTitle(roots []string, cwd, sessionID string) string {
	for _, projectsDir := range roots {
		// First root that answers for the session wins, titled or not.
		if title, stop := projectTitleInRoot(projectsDir, cwd, sessionID); stop {
			return title
		}
	}
	return ""
}

// projectTitleInRoot reports the session's title within one root, and whether
// this root ends the search. The two are distinct: a session that lives here
// but has not been titled yet is ("", true) and must end the search, while a
// session that lives in another profile is ("", false) and must not.
func projectTitleInRoot(projectsDir, cwd, sessionID string) (title string, stop bool) {
	projectDir := findProjectDir(projectsDir, cwd)
	if projectDir == "" {
		return "", false
	}

	sessionFile := filepath.Join(projectDir, sessionID+".jsonl")
	if _, err := os.Stat(sessionFile); err != nil {
		if sessionID != "" {
			return "", false
		}
		// A pane that reported no session id can still be identified when the
		// project directory holds exactly one transcript. That heuristic is
		// only sound as a same-root guess: there is no reader to agree with
		// (it rejects empty session ids outright), so this root owning the
		// cwd's project directory is the only anchor tying the guess to the
		// pane. When the directory is ambiguous the search must end empty
		// rather than fall through to a different tree, whose sole transcript
		// is some unrelated session - a confidently wrong title over a
		// conversation view that says "not available".
		entries, err := os.ReadDir(projectDir)
		if err != nil {
			return "", true
		}
		var jsonlFiles []string
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
				jsonlFiles = append(jsonlFiles, e.Name())
			}
		}
		if len(jsonlFiles) != 1 {
			return "", true
		}
		sessionFile = filepath.Join(projectDir, jsonlFiles[0])
	}

	return extractTitle(sessionFile), true
}

func findProjectDir(projectsDir, cwd string) string {
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return ""
	}

	encoded := encodePath(cwd)
	leadingDashEncoded := "-" + encoded
	for _, e := range entries {
		if e.Name() != encoded && e.Name() != leadingDashEncoded {
			continue
		}
		if dir := filepath.Join(projectsDir, e.Name()); entryIsDir(e, dir) {
			return dir
		}
	}

	for _, e := range entries {
		dir := filepath.Join(projectsDir, e.Name())
		if entryIsDir(e, dir) && matchesCwd(dir, cwd) {
			return dir
		}
	}
	return ""
}

// entryIsDir classifies a directory entry by what it points at, not by its own
// type bits. os.ReadDir reports ModeSymlink for a symlink, so DirEntry.IsDir()
// is false for a symlink to a directory and a project directory symlinked in
// from a dotfiles repo would be silently skipped; agentroots.profileAgentDirs
// documents the same hazard at length. Only a symlink needs the stat: the type
// bits already settle every other entry.
func entryIsDir(e os.DirEntry, path string) bool {
	if e.IsDir() {
		return true
	}
	if e.Type()&os.ModeSymlink == 0 {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func codexSessionName(codexHomes []string, sessionID string) string {
	for _, codexHome := range codexHomes {
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

// sourceSignature fingerprints every file the name could have been read from,
// so an edit to any of them invalidates the cached entry. roots is the list
// SessionName already resolved for this agent, and sessionPath the transcript
// an OMP or Pi pane reported: re-resolving either here would scan the profiles
// directory a second time and could sign a different tree than the one the
// title came from.
func sourceSignature(agentLower, cwd, sessionID, sessionPath string, roots []string) string {
	if isOMPSessionAgent(agentLower) || isPiSessionAgent(agentLower) {
		return pathSignature(sessionPath)
	}
	if strings.Contains(agentLower, "codex") {
		return rootsSignature(roots, "session_index.jsonl")
	}
	if !strings.Contains(agentLower, "qoder") && !strings.Contains(agentLower, "claude") {
		return ""
	}
	// Sign the candidate in EVERY root, never just the first root that happens
	// to have a project directory for this cwd. The title comes from the first
	// root that HOLDS the session, which can be any of them and changes the
	// moment a copy appears in an earlier one, so stopping here at an earlier
	// root would sign a file the title did not come from and pin a stale title
	// for the whole cache TTL. With exactly one root - every default,
	// unconfigured install - this reduces to the original signature.
	signatures := make([]string, 0, len(roots))
	for _, projectsDir := range roots {
		projectDir := findProjectDir(projectsDir, cwd)
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
// empty), joined in root order. agentroots reports the roots in a fixed order,
// so the result is deterministic.
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
