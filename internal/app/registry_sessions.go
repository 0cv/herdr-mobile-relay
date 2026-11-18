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

	"github.com/0cv/herdr-mobile-relay/internal/agentkind"
	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
)

const maxSessionRegistry = 1 << 20

type registryEntry struct {
	Session string  `json:"session_id"`
	Surface string  `json:"surface"`
	PID     int     `json:"pid"`
	Start   float64 `json:"process_start_time"`
}

func parseRegistry(data []byte, agent string) ([]registryEntry, error) {
	if agent == agentkind.Hermes {
		var registry struct {
			Entries []registryEntry `json:"entries"`
		}
		err := json.Unmarshal(data, &registry)
		return registry.Entries, err
	}
	var rows []struct {
		Session string `json:"session_id"`
		PID     int    `json:"pid"`
		Start   int64  `json:"pid_start"`
	}
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, err
	}
	entries := make([]registryEntry, 0, len(rows))
	for _, row := range rows {
		entries = append(entries, registryEntry{Session: row.Session, Surface: "cli", PID: row.PID, Start: float64(row.Start) / 1e6})
	}
	return entries, nil
}

func readRegistryEntries(paths []string, agent string) ([]registryEntry, error) {
	var entries []registryEntry
	for _, path := range paths {
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Size() > maxSessionRegistry {
			return nil, io.ErrUnexpectedEOF
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(f, maxSessionRegistry+1))
		f.Close()
		if err != nil {
			return nil, err
		}
		if len(data) > maxSessionRegistry {
			return nil, io.ErrUnexpectedEOF
		}
		rows, err := parseRegistry(data, agent)
		if err != nil {
			return nil, err
		}
		entries = append(entries, rows...)
	}
	return entries, nil
}

func matchRegistrySession(entries []registryEntry, pid int, start float64) string {
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

func (s *Server) enrichRegistrySessions(ctx context.Context, agents []*coordinator.AgentState) {
	if s.cfg.Backend != "hc" {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	loaded := make(map[string][]registryEntry)
	for _, a := range agents {
		if a == nil || a.Session != "" {
			continue
		}
		agent := agentkind.Of(a.Agent)
		if agent != agentkind.Hermes && agent != agentkind.Grok {
			continue
		}
		entries, ok := loaded[agent]
		if !ok {
			paths := []string{filepath.Join(home, ".grok", "active_sessions.json")}
			if agent == agentkind.Hermes {
				paths = nil
				for _, root := range agentroots.HermesData(home) {
					paths = append(paths, filepath.Join(root, "runtime", "active_sessions.json"))
				}
			}
			entries, err = readRegistryEntries(paths, agent)
			if err != nil {
				entries = nil
			}
			loaded[agent] = entries
		}
		if len(entries) == 0 {
			continue
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
			start, err := registryProcessStart(ctx, pid)
			if err != nil {
				return
			}
			a.Session = matchRegistrySession(entries, pid, start)
		}()
	}
}
