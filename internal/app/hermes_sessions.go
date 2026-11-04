package app

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/slashcmd"
)

const maxHermesRegistry = 1 << 20

type hermesEntry struct {
	Session string  `json:"session_id"`
	Surface string  `json:"surface"`
	PID     int     `json:"pid"`
	Start   float64 `json:"process_start_time"`
}

func readHermesEntries(roots []string) ([]hermesEntry, error) {
	var entries []hermesEntry
	for _, root := range roots {
		path := filepath.Join(root, "runtime", "active_sessions.json")
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Size() > maxHermesRegistry {
			return nil, io.ErrUnexpectedEOF
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(f, maxHermesRegistry+1))
		f.Close()
		if err != nil {
			return nil, err
		}
		if len(data) > maxHermesRegistry {
			return nil, io.ErrUnexpectedEOF
		}
		var registry struct {
			Entries []hermesEntry `json:"entries"`
		}
		if err := json.Unmarshal(data, &registry); err != nil {
			return nil, err
		}
		entries = append(entries, registry.Entries...)
	}
	return entries, nil
}

func matchHermesSession(entries []hermesEntry, pid int, start float64) string {
	if pid <= 0 || start <= 0 {
		return ""
	}
	session := ""
	matches := 0
	for _, entry := range entries {
		if entry.Surface != "cli" || entry.PID != pid || entry.Start <= 0 || math.Abs(entry.Start-start) >= 0.001 {
			continue
		}
		matches++
		session = entry.Session
	}
	if matches != 1 || strings.TrimSpace(session) == "" {
		return ""
	}
	return session
}

func (s *Server) enrichHermesSessions(ctx context.Context, agents []*coordinator.AgentState) {
	if s.cfg.Backend != "hc" {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	var entries []hermesEntry
	loaded := false
	for _, a := range agents {
		if a == nil || slashcmd.ProfileIDForAgentName(a.Agent) != "hermes" || a.Session != "" {
			continue
		}
		if !loaded {
			entries, err = readHermesEntries(agentroots.HermesData(home))
			if err != nil {
				return
			}
			loaded = true
		}
		func() {
			ctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
			defer cancel()
			info, err := s.herdrC.PaneProcessInfoFresh(ctx, a.PaneID)
			if err != nil {
				return
			}
			pid, err := foregroundRoot(info, a.PaneID)
			if err != nil {
				return
			}
			start, err := hermesProcessStart(ctx, pid)
			if err != nil {
				return
			}
			a.Session = matchHermesSession(entries, pid, start)
		}()
	}
}
