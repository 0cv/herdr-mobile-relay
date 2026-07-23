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

func (m *Manager) Merge(paneID string, rawContent string) string {
	m.mu.Lock()
	defer m.mu.Unlock()

	state := m.loadState(paneID)

	body, footer := splitSnapshot(rawContent)
	if len(body) == 0 {
		return joinContent(state)
	}

	hash := hashLines(body)
	if hash == state.LastHash && state.StaleRefusals == 0 {
		return joinContent(state)
	}
	state.LastHash = hash
	state.Footer = footer

	normalized := normalizeLines(body)
	histNorm := normalizeLines(state.History)

	if overlap := tailOverlap(histNorm, normalized); overlap > 0 {
		state.History = append(state.History, body[overlap:]...)
		state.StaleRefusals = 0
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
	return joinContent(state)
}

func (m *Manager) Content(paneID string, limit int) string {
	m.mu.Lock()
	defer m.mu.Unlock()

	state := m.loadState(paneID)
	content := joinContent(state)
	lines := strings.Split(content, "\n")
	if limit > 0 && len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	return strings.Join(lines, "\n")
}

func (m *Manager) Discard(paneID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.states, paneID)
	os.Remove(m.stateFile(paneID))
}

func (m *Manager) SaveAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for paneID, state := range m.states {
		m.saveState(paneID, state)
	}
}

func (m *Manager) applyMatch(state *PaneState, body []string, match seqmatch.Match) {
	historyEnd := match.A + match.Size
	currentEnd := match.B + match.Size
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

func joinContent(state *PaneState) string {
	var parts []string
	if len(state.History) > 0 {
		parts = append(parts, strings.Join(state.History, "\n"))
	}
	if len(state.Footer) > 0 {
		parts = append(parts, strings.Join(state.Footer, "\n"))
	}
	return strings.Join(parts, "\n")
}
