package history

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/seqmatch"
)

const (
	MaxLines        = 10000
	FooterLines     = 6
	CaptureInterval = 4 * time.Second
	SaveInterval    = 10 * time.Second
)

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]|\x1b\][^\x07]*\x07|\x1b[()][0-9A-B]|\x1b[>=<]|\x1b\[[\?]?[0-9;]*[hlJKHfG]`)

type PaneState struct {
	History       []string `json:"history"`
	Footer        []string `json:"footer"`
	StaleRefusals int      `json:"stale_refusals"`
	LastHash      string   `json:"last_hash"`
}

type Manager struct {
	mu       sync.Mutex
	states   map[string]*PaneState
	dir      string
	lastSave map[string]time.Time
}

func NewManager(cacheDir string) *Manager {
	dir := filepath.Join(cacheDir, "claude-history")
	os.MkdirAll(dir, 0o700)
	return &Manager{
		states:   make(map[string]*PaneState),
		dir:      dir,
		lastSave: make(map[string]time.Time),
	}
}

// Merge appends the latest pane snapshot to its stored history.
func (m *Manager) Merge(paneID string, rawContent string) string {
	content, _ := m.merge(paneID, rawContent, 0)
	return content
}

// MergeLimited retains the complete pane history while returning only its latest lines.
func (m *Manager) MergeLimited(paneID string, rawContent string, limit int) (string, bool) {
	return m.merge(paneID, rawContent, limit)
}

func (m *Manager) merge(paneID string, rawContent string, limit int) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	state := m.loadState(paneID)

	body, footer := splitSnapshot(rawContent)
	if len(body) == 0 {
		return joinContent(state, limit)
	}

	hash := hashLines(body)
	if hash == state.LastHash && state.StaleRefusals == 0 {
		return joinContent(state, limit)
	}
	state.LastHash = hash
	state.Footer = footer

	normalized := normalizeLines(body)
	histNorm := normalizeLines(state.History)

	if overlap := tailOverlap(histNorm, normalized); overlap > 0 {
		historyStart := len(state.History) - overlap
		copy(state.History[historyStart:], body[:overlap])
		state.History = append(state.History, body[overlap:]...)
	} else if match := sequenceMatch(histNorm, normalized); match.Size >= 2 {
		m.applyMatch(state, body, match)
	} else {
		state.History = append(state.History, body...)
		state.StaleRefusals = 0
	}

	if len(state.History) > MaxLines {
		state.History = state.History[len(state.History)-MaxLines:]
	}

	m.maybeSave(paneID, state)
	return joinContent(state, limit)
}

// Content returns the pane's stored history clipped to its newest limit lines,
// and whether clipping dropped anything - the same contract as MergeLimited,
// because a caller that serves this to a client has to tell it the transcript
// continues above what it received.
func (m *Manager) Content(paneID string, limit int) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return joinContent(m.loadState(paneID), limit)
}

// Depth reports how many history rows are stored for a pane, excluding the
// footer. Callers use it to decide whether a pane still needs a seed.
func (m *Manager) Depth(paneID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.loadState(paneID).History)
}

// Seed installs a transcript harvested from the pane's own scrollback beneath
// the rows the relay watched. An alternate-screen agent keeps no scrollback a
// display read can reach, so without a seed a pane's history begins at whatever
// screen the relay happened to see first. The harvest ends at the pane's current
// screen, so its tail is the same content the stored history already holds:
// the overlap is dropped rather than duplicated, and the stored rows win because
// they carry the ANSI styling the harvested text lost.
//
// Seeding is a store, not a read: it returns nothing because joining up to
// MaxLines rows into one string costs an allocation and a copy of the whole
// transcript, and a seed happens once per pane rather than per frame. Callers
// that want the result read it back with Content.
func (m *Manager) Seed(paneID string, rawContent string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	state := m.loadState(paneID)
	body, footer := splitSnapshot(rawContent)
	if len(body) == 0 {
		return
	}
	if len(state.History) == 0 {
		state.History = body
		if len(state.Footer) == 0 {
			state.Footer = footer
		}
	} else if older := seedPrefix(body, state.History); len(older) > 0 {
		state.History = append(older, state.History...)
	}
	if len(state.History) > MaxLines {
		state.History = state.History[len(state.History)-MaxLines:]
	}

	m.maybeSave(paneID, state)
}

// seedPrefix returns the seed rows that precede the stored history. The exact
// tail-to-head overlap is tried first; a partial match anywhere is the fallback,
// because a pane read at one width and harvested at another reflows its own
// output and only some rows survive verbatim.
func seedPrefix(seed, history []string) []string {
	seedNorm := normalizeLines(seed)
	histNorm := normalizeLines(history)
	if overlap := tailOverlap(seedNorm, histNorm); overlap > 0 {
		return seed[:len(seed)-overlap]
	}
	match := sequenceMatch(seedNorm, histNorm)
	if match.Size < 2 {
		return seed
	}
	if match.A <= match.B {
		return nil
	}
	return seed[:match.A-match.B]
}

func (m *Manager) Discard(paneID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.states, paneID)
	delete(m.lastSave, paneID)
	os.Remove(m.stateFile(paneID))
}

func (m *Manager) SaveAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for paneID, state := range m.states {
		m.saveState(paneID, state)
	}
}

// Reconcile removes persisted and in-memory history for panes that are absent
// from a successful inventory snapshot. It does not need to decode pane IDs
// from filenames; active panes are mapped through the same canonical filename
// function used for persistence.
func (m *Manager) Reconcile(activePaneIDs map[string]bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	activeFiles := make(map[string]bool, len(activePaneIDs))
	for paneID := range activePaneIDs {
		activeFiles[filepath.Base(m.stateFile(paneID))] = true
	}
	entries, err := os.ReadDir(m.dir)
	if err == nil {
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || activeFiles[entry.Name()] {
				continue
			}
			_ = os.Remove(filepath.Join(m.dir, entry.Name()))
		}
	}
	for paneID := range m.states {
		if activePaneIDs[paneID] {
			continue
		}
		delete(m.states, paneID)
		delete(m.lastSave, paneID)
	}
}

func (m *Manager) applyMatch(state *PaneState, body []string, match seqmatch.Match) {
	historyEnd := match.A + match.Size
	currentEnd := match.B + match.Size
	copy(state.History[match.A:historyEnd], body[match.B:currentEnd])
	currentSuffix := body[currentEnd:]
	historyTail := len(state.History) - historyEnd

	switch {
	case len(currentSuffix) == 0:
		// Scrolled-up viewport re-showing known content
		state.StaleRefusals = 0
	case historyTail >= len(body):
		// Match implausibly deep — treat whole frame as new
		state.History = append(state.History, body...)
		state.StaleRefusals = 0
	case historyTail <= 3:
		// Normal case — rebase
		state.History = append(state.History[:historyEnd], currentSuffix...)
		state.StaleRefusals = 0
	default:
		// Ambiguous divergent tail — use stale refusal counter
		state.StaleRefusals++
		if state.StaleRefusals >= 2 {
			state.History = append(state.History[:historyEnd], currentSuffix...)
			state.StaleRefusals = 0
		}
	}
}

func (m *Manager) loadState(paneID string) *PaneState {
	if s, ok := m.states[paneID]; ok {
		return s
	}

	s := &PaneState{}
	data, err := os.ReadFile(m.stateFile(paneID))
	if err == nil {
		json.Unmarshal(data, s)
	}
	m.states[paneID] = s
	return s
}

func (m *Manager) maybeSave(paneID string, state *PaneState) {
	if t, ok := m.lastSave[paneID]; ok && time.Since(t) < SaveInterval {
		return
	}
	m.saveState(paneID, state)
	m.lastSave[paneID] = time.Now()
}

func (m *Manager) saveState(paneID string, state *PaneState) {
	data, err := json.Marshal(state)
	if err != nil {
		return
	}
	path := m.stateFile(paneID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	os.Rename(tmp, path)
}

func (m *Manager) stateFile(paneID string) string {
	safe := strings.ReplaceAll(paneID, "/", "_")
	safe = strings.ReplaceAll(safe, ":", "_")
	return filepath.Join(m.dir, safe+".json")
}

func splitSnapshot(raw string) (body, footer []string) {
	lines := strings.Split(raw, "\n")
	if len(lines) <= FooterLines {
		return lines, nil
	}
	return lines[:len(lines)-FooterLines], lines[len(lines)-FooterLines:]
}

func normalizeLines(lines []string) []string {
	result := make([]string, len(lines))
	for i, line := range lines {
		result[i] = NormalizeLine(line)
	}
	return result
}

func NormalizeLine(line string) string {
	line = ansiRe.ReplaceAllString(line, "")
	line = strings.ReplaceAll(line, "\r", "")
	return strings.TrimRight(line, " \t\n")
}

func tailOverlap(history, current []string) int {
	maxK := len(history)
	if len(current) < maxK {
		maxK = len(current)
	}
	for k := maxK; k > 0; k-- {
		match := true
		for i := 0; i < k; i++ {
			if history[len(history)-k+i] != current[i] {
				match = false
				break
			}
		}
		if match {
			return k
		}
	}
	return 0
}

func sequenceMatch(history, current []string) seqmatch.Match {
	if len(history) == 0 || len(current) == 0 {
		return seqmatch.Match{}
	}

	m := seqmatch.NewMatcher(history, current)
	blocks := m.GetMatchingBlocks()

	var best seqmatch.Match
	for _, block := range blocks {
		if block.Size < 2 {
			continue
		}
		nonEmpty := 0
		for i := 0; i < block.Size; i++ {
			if history[block.A+i] != "" {
				nonEmpty++
			}
		}
		if nonEmpty < 2 {
			continue
		}
		if block.Size > best.Size || (block.Size == best.Size && block.A > best.A) {
			best = block
		}
	}
	return best
}

func hashLines(lines []string) string {
	h := uint64(0)
	for _, l := range lines {
		for _, c := range l {
			h = h*31 + uint64(c)
		}
		h = h*31 + uint64('\n')
	}
	return fmt.Sprintf("%016x", h)
}

func joinContent(state *PaneState, limit int) (string, bool) {
	historyLines := state.History
	footerLines := state.Footer
	truncated := limit > 0 && len(historyLines)+len(footerLines) > limit
	if truncated {
		if len(footerLines) >= limit {
			historyLines = nil
			footerLines = footerLines[len(footerLines)-limit:]
		} else {
			historyLines = historyLines[len(historyLines)-(limit-len(footerLines)):]
		}
	}

	var parts []string
	if len(historyLines) > 0 {
		parts = append(parts, strings.Join(historyLines, "\n"))
	}
	if len(footerLines) > 0 {
		parts = append(parts, strings.Join(footerLines, "\n"))
	}
	return strings.Join(parts, "\n"), truncated
}
