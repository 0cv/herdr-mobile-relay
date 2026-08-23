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

// NewResolver keeps the home directory and a conversation.Reader built on the
// same home (NewReader stores home and nothing else), so construction reads
// nothing. With a session id the transcript is located by the reader's own
// Locate - the very function the conversation view resolves through - so the
// file a pane's title is read from and the transcript the reader serves come
// out of one lookup instead of two that must agree: a split between them is
// not expressible, for any agent kind, and an id the reader refuses is never
// titled. The one lookup with no reader counterpart is the empty-session-id
// heuristic; see soleTranscriptInRoot for why it is a deliberate exception.
//
// No root list is resolved here. agentroots discovers Pi and Oh My Pi named
// profiles by reading <config root>/profiles, and the relay is a long-lived
// user service: a profile directory appears the first time someone runs the
// agent under that profile, which is almost always after the relay started. A
// snapshot taken at construction would leave every pane in that profile
// unresolvable until the service was restarted. Locate resolves its lists per
// call for the same reason.
func NewResolver(home string) *Resolver {
	return &Resolver{cache: make(map[string]cacheEntry), home: home, reader: conversation.NewReader(home)}
}

func (r *Resolver) claudeRoots() []string { return agentroots.Claude(r.home) }
func (r *Resolver) qoderRoots() []string  { return agentroots.Qoder(r.home) }

// heuristicRoots reports the cwd-encoded project-directory roots the
// empty-session-id heuristic scans. Only Claude and Qoder keep such trees;
// every other agent's pane is never titled without an id.
func (r *Resolver) heuristicRoots(kind agentKind) []string {
	if kind == agentQoder {
		return r.qoderRoots()
	}
	return r.claudeRoots()
}

func (r *Resolver) SessionName(agent, cwd, sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	kind := classifyAgent(agent)
	if kind == agentUnknown {
		return ""
	}

	// ONE lookup per call produces both the file the title will be read from
	// and the cache signature over it, so the signature always signs the
	// title's actual source; deriving the two through separate scans is how
	// they once came to disagree. No root list is resolved or scanned twice:
	// with a session id the only scan is Locate's own, and the Codex index
	// home falls out of the root Locate already answered with.
	var titleSource, sig string
	codexIndex := ""
	if sessionID == "" {
		// The reader rejects empty ids outright, so there is no lookup to
		// share; see soleTranscriptInRoot for the one resolver-only guess.
		if kind != agentQoder && kind != agentClaude {
			return ""
		}
		titleSource, sig = resolveSoleTranscript(r.heuristicRoots(kind), cwd)
	} else if path, root, ok := r.reader.Locate(agent, sessionID); ok {
		titleSource = path
		sig = pathSignature(path)
		if kind == agentCodex {
			// The title must be read from the SAME home whose rollout the
			// reader serves. Codex writes the index record before it names
			// the thread, so an empty thread_name here is the ordinary
			// fresh-session state - answering with another home's non-empty
			// name instead would put that home's title over this home's
			// transcript. Locate's root is agentroots.Codex's element,
			// <home>/sessions in the configured spelling, so its parent is
			// the home whose index belongs to this rollout.
			codexIndex = filepath.Join(filepath.Dir(root), "session_index.jsonl")
			sig += "|" + pathSignature(codexIndex)
		}
	} else {
		// No transcript for this pane means no title: a confident name over
		// a conversation view that says "not available" is worse than none.
		// A miss has no file to sign, so a cached miss lives out its TTL.
		sig = "locate-miss"
	}

	key := agent + "|" + cwd + "|" + sessionID
	r.mu.Lock()
	if entry, ok := r.cache[key]; ok && entry.sig == sig && time.Now().Before(entry.expires) {
		r.mu.Unlock()
		return entry.name
	}
	r.mu.Unlock()

	var name string
	if titleSource != "" {
		switch kind {
		case agentOMP:
			name = extractOMPSessionTitle(titleSource)
		case agentPi:
			name = extractPiSessionTitle(titleSource)
		case agentQoder, agentClaude:
			name = extractTitle(titleSource)
		case agentCodex:
			name = codexIndexThreadName(codexIndex, sessionID)
		}
	}

	r.mu.Lock()
	r.cache[key] = cacheEntry{name: name, expires: time.Now().Add(cacheTTL), sig: sig}
	r.mu.Unlock()

	return name
}

// agentKind is the single classification every resolver-side dispatch
// switches on: the title-extraction strategy, the empty-session-id
// heuristic's gate and its root choice all branch on the same value, so
// their notions of "which agent is this" cannot drift from each other.
// Whether a session id has a transcript is not dispatched on it at all -
// that is conversation.Reader.Locate's classification; see classifyAgent.
type agentKind int

const (
	agentUnknown agentKind = iota
	agentOMP
	agentPi
	agentQoder
	agentClaude
	agentCodex
)

// classifyAgent picks the title-extraction strategy: Pi and Oh My Pi by exact
// name, everything else by substring. It deliberately does NOT decide whether
// a session id has a transcript - conversation.Reader.Locate does, with the
// reader's own exact normalized-name classification - so a name this matches
// but the reader does not ("claude-work") is refused a title by Locate rather
// than titled with no transcript source, exactly as the reader refuses it a
// conversation. The residual asymmetry runs the safe way only: a name the
// reader supports but this misses (say "pi_coding_agent", which normalizes to
// a supported name yet matches no case below) goes untitled, never mistitled
// - and herdr reports fixed kinds ("claude", "codex", "qodercli", "pi",
// "omp"; profiles.isHerdrKind), which both sides classify identically. The
// substring cases still gate the empty-session-id heuristic, where the reader
// offers no classification to share because it rejects the id outright.
func classifyAgent(agent string) agentKind {
	agent = strings.ToLower(strings.TrimSpace(agent))
	switch agent {
	case "omp", "oh-my-pi", "oh my pi", "ohmypi":
		return agentOMP
	case "pi", "pi-coding-agent":
		return agentPi
	}
	switch {
	case strings.Contains(agent, "qoder"):
		return agentQoder
	case strings.Contains(agent, "claude"):
		return agentClaude
	case strings.Contains(agent, "codex"):
		return agentCodex
	}
	return agentUnknown
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

// resolveSoleTranscript is the empty-session-id path: it reports the
// heuristic's transcript guess - "" when no root answers - together with the
// cache signature over every path that decision depended on. Both come out of
// one soleTranscriptInRoot call per root, so the signature always signs the
// file the title came from.
//
// The FIRST root that owns the cwd's project directory ends the search,
// titled or not; roots after it are never scanned, so nothing in them needs
// signing either. See soleTranscriptInRoot for why falling through to a later
// tree would be wrong.
func resolveSoleTranscript(roots []string, cwd string) (path, sig string) {
	signatures := make([]string, 0, len(roots))
	for _, projectsDir := range roots {
		candidate, rootSig, stop := soleTranscriptInRoot(projectsDir, cwd)
		signatures = append(signatures, rootSig)
		if stop {
			return candidate, strings.Join(signatures, "|")
		}
	}
	return "", strings.Join(signatures, "|")
}

// soleTranscriptInRoot is the empty-session-id heuristic: a pane that
// reported no session id can still be identified when the cwd's project
// directory holds exactly one transcript. It is the one lookup in this file
// that does NOT route through conversation.Reader.Locate, deliberately: the
// reader rejects empty ids outright, so there is no transcript it could
// serve and therefore none this guess could ever contradict - the shared
// seam has nothing to share here. The guess is only sound within the root
// that owns the cwd's project directory, so owning it ends the search
// whatever it holds: when the directory is ambiguous the search ends empty
// rather than falling through to a different tree, whose sole transcript is
// some unrelated session - a confidently wrong title over a conversation
// view that says "not available".
//
// Candidates get the same acceptance the id path's transcripts get:
// classified through entryIsDir (a symlink to a directory is not a
// transcript, however it is named) and read only when Stat reports a regular
// file.
func soleTranscriptInRoot(projectsDir, cwd string) (path, sig string, stop bool) {
	projectDir := findProjectDir(projectsDir, cwd)
	if projectDir == "" {
		return "", pathSignature(projectsDir), false
	}
	entries, err := os.ReadDir(projectDir)
	if err != nil {
		return "", pathSignature(projectDir), true
	}
	// The guess depends on the whole listing: a second transcript appearing
	// must invalidate the cache, not just an edit to the chosen one.
	signatures := []string{pathSignature(projectDir)}
	var transcripts []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		p := filepath.Join(projectDir, e.Name())
		if entryIsDir(e, p) {
			continue
		}
		info, statErr := os.Stat(p)
		signatures = append(signatures, statSignature(p, info, statErr))
		if statErr != nil || !info.Mode().IsRegular() {
			continue
		}
		transcripts = append(transcripts, p)
	}
	sig = strings.Join(signatures, "|")
	if len(transcripts) != 1 {
		return "", sig, true
	}
	return transcripts[0], sig, true
}

// findProjectDir locates the cwd-encoded project directory (or one whose cwd
// file names the cwd) within one root. Only the empty-session-id heuristic
// still needs it: with a session id the search key is the id itself, shared
// with conversation.Reader, and neither the cwd nor the per-entry cwd-file
// read below plays any part.
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

// codexIndexThreadName reads one home's session_index.jsonl. The caller is
// responsible for handing it the index of the home whose rollout
// conversation.Reader.Locate answered with - never another home's, whose
// record for the same id names a different conversation.
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

func pathSignature(path string) string {
	info, err := os.Stat(path)
	return statSignature(path, info, err)
}

// statSignature is pathSignature for a caller that already holds the Stat
// result, so a probe that decided a candidate is not paid twice.
func statSignature(path string, info os.FileInfo, err error) string {
	if err != nil {
		return path + "|missing"
	}
	return path + "|" + strconv.FormatInt(info.ModTime().UnixNano(), 10) + "|" + strconv.FormatInt(info.Size(), 10)
}
