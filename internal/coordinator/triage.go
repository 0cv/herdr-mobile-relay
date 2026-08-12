package coordinator

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	triageStateVersion  = 2
	triageStateFilename = "triage-state.json"
	triageStateLimit    = 2048
	triageStateMaxAge   = 90 * 24 * time.Hour
)

type triageRecord struct {
	LastActiveAt int64 `json:"last_active_at,omitempty"`
	LastSeenAt   int64 `json:"last_seen_at,omitempty"`
}

type triageStateFile struct {
	Version int                     `json:"version"`
	Panes   map[string]triageRecord `json:"panes"`
}

// EnableTriagePersistence loads durable per-pane activity and seen timestamps.
// The state is advisory: a missing or corrupt file never blocks relay startup.
func (s *State) EnableTriagePersistence(cacheDir string) error {
	if strings.TrimSpace(cacheDir) == "" {
		return errors.New("triage cache directory is required")
	}
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(cacheDir, triageStateFilename)
	loaded := make(map[string]triageRecord)
	migrated := false
	data, err := os.ReadFile(path)
	if err == nil {
		var file triageStateFile
		if json.Unmarshal(data, &file) != nil ||
			(file.Version != 1 && file.Version != triageStateVersion) {
			return errors.New("triage state is invalid")
		}
		migrated = file.Version == 1
		for key, record := range file.Panes {
			if key == "" || record.LastActiveAt < 0 || record.LastSeenAt < 0 {
				continue
			}
			if migrated {
				record.LastActiveAt = 0
			}
			loaded[key] = record
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.triagePath = path
	s.triage = loaded
	if migrated {
		s.persistTriageLocked()
	}
	return nil
}

func triageIdentity(agent *AgentState) string {
	if agent == nil {
		return ""
	}
	parts := make([]string, 0, 5)
	if agent.TerminalID != "" {
		parts = append(parts, "terminal="+agent.TerminalID)
	} else {
		parts = append(parts,
			"pane="+firstNonEmpty(agent.RawPaneID, agent.PaneID),
			"tab="+agent.TabID,
			"workspace="+agent.WorkspaceID,
		)
	}
	if agent.SessionID != "" {
		parts = append(parts, "session="+agent.SessionID)
	}
	if len(parts) == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func (s *State) applyTriageLocked(agent *AgentState) {
	if agent == nil {
		return
	}
	record := s.triage[triageIdentity(agent)]
	agent.LastActiveAt = record.LastActiveAt
	agent.LastSeenAt = record.LastSeenAt
}

func (s *State) syncTriageLocked(agent *AgentState) bool {
	key := triageIdentity(agent)
	if key == "" {
		return false
	}
	next := triageRecord{LastActiveAt: agent.LastActiveAt, LastSeenAt: agent.LastSeenAt}
	if next.LastActiveAt == 0 && next.LastSeenAt == 0 {
		return false
	}
	if current, ok := s.triage[key]; ok && current == next {
		return false
	}
	s.triage[key] = next
	return true
}

func (s *State) persistTriageLocked() {
	if s.triagePath == "" {
		return
	}
	s.pruneTriageLocked(time.Now().Add(-triageStateMaxAge).UnixMilli())
	data, err := json.Marshal(triageStateFile{Version: triageStateVersion, Panes: s.triage})
	if err != nil {
		s.logger.Warn("triage state could not be encoded", "error", err)
		return
	}
	temp := s.triagePath + ".tmp"
	if err := os.WriteFile(temp, data, 0o600); err != nil {
		s.logger.Warn("triage state could not be written", "error", err)
		return
	}
	if err := os.Rename(temp, s.triagePath); err != nil {
		_ = os.Remove(temp)
		s.logger.Warn("triage state could not be activated", "error", err)
	}
}

func (s *State) pruneTriageLocked(cutoff int64) {
	for key, record := range s.triage {
		if maxInt64(record.LastActiveAt, record.LastSeenAt) < cutoff {
			delete(s.triage, key)
		}
	}
	if len(s.triage) <= triageStateLimit {
		return
	}
	type candidate struct {
		key string
		ts  int64
	}
	ordered := make([]candidate, 0, len(s.triage))
	for key, record := range s.triage {
		ordered = append(ordered, candidate{key: key, ts: maxInt64(record.LastActiveAt, record.LastSeenAt)})
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ts < ordered[j].ts })
	for _, item := range ordered[:len(ordered)-triageStateLimit] {
		delete(s.triage, item.key)
	}
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}
