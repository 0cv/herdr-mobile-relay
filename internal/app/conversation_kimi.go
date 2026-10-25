package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/conversation"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

const maxKimiProcessChecks = 32

func (s *Server) kimiProcess(ctx context.Context, pane string) (string, string, error) {
	birth := s.kimiProcessBirth
	if birth == nil {
		birth = kimiProcessStart
	}
	return kimiVerifiedProcess(ctx, s.herdrC, pane, birth)
}

func kimiVerifiedProcess(ctx context.Context, reader paneProcessReader, pane string, birth func(int) (string, error)) (string, string, error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	info, err := reader.PaneProcessInfoFresh(ctx, pane)
	if err != nil {
		return "", "", err
	}
	identity, cwd, err := kimiProcessIdentity(info, pane)
	if err != nil {
		return "", "", err
	}
	pid, _ := foregroundRoot(info, pane)
	start, startErr := birth(pid)
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	after, err := reader.PaneProcessInfoFresh(ctx, pane)
	if err != nil {
		return "", "", err
	}
	current, _, err := kimiProcessIdentity(after, pane)
	end, endErr := birth(pid)
	if err != nil || current != identity || ctx.Err() != nil {
		return "", "", errors.New("Kimi foreground process changed")
	}
	if startErr != nil || start == "" || endErr != nil || end == "" {
		return "unverified:" + identity, cwd, nil
	}
	if start != end {
		return "", "", errors.New("Kimi foreground process changed")
	}
	return "verified:" + start + ":" + identity, cwd, nil
}

func kimiProcessIdentity(info *herdr.PaneProcessInfo, pane string) (string, string, error) {
	pid, err := foregroundRoot(info, pane)
	if err != nil {
		return "", "", err
	}
	for _, process := range info.ForegroundProcesses {
		if process.PID != pid || !filepath.IsAbs(process.Cwd) {
			continue
		}
		encoded, _ := json.Marshal(struct {
			CWD  string
			Argv []string
		}{process.Cwd, process.Argv})
		identity := fmt.Sprintf("%d:%x", pid, sha256.Sum256(encoded))
		return identity, process.Cwd, nil
	}
	return "", "", errors.New("Kimi foreground process is unavailable")
}

func kimiPeerConflicts(cwd, process, freshCWD string, err error) bool {
	return err != nil || process == "" || !filepath.IsAbs(freshCWD) || kimiDirectory(freshCWD) == cwd
}

func (s *Server) enrichKimiSessions(ctx context.Context, agents []*coordinator.AgentState) {
	check, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	checked := 0
	for _, agent := range agents {
		if !conversation.IsKimi(agent.Agent) {
			continue
		}
		agent.ConversationProcess, agent.ForegroundCwd = "", ""
		checked++
		if checked > maxKimiProcessChecks || check.Err() != nil {
			agent.ConversationReason = "ambiguous_session"
			continue
		}
		process, cwd, err := s.kimiProcess(check, agent.PaneID)
		if err != nil {
			agent.ConversationReason = "source_unavailable"
			continue
		}
		agent.ConversationProcess, agent.ForegroundCwd = process, cwd
	}
	s.resolveKimiSessions(agents)
}

func (s *Server) kimiSelectionCurrent(ctx context.Context, agent *coordinator.AgentState) bool {
	if !conversation.IsKimi(agent.Agent) || !agent.ConversationInferred && agent.ConversationProcess == "" {
		return true
	}
	check, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	code := ""
	process, cwd, err := s.kimiProcess(check, agent.PaneID)
	if err != nil || agent.ConversationProcess == "" && agent.SessionID != "" {
		code = "source_unavailable"
	} else if agent.ConversationProcess != "" && process != agent.ConversationProcess {
		code = "source_changed"
	}
	if agent.ConversationInferred && code == "" {
		checked := 0
		for _, other := range s.state.Snapshot() {
			if other.PaneID == agent.PaneID || !conversation.IsKimi(other.Agent) {
				continue
			}
			checked++
			if checked > maxKimiProcessChecks || check.Err() != nil {
				code = "ambiguous_session"
				break
			}
			otherProcess, otherCWD, otherErr := s.kimiProcess(check, other.PaneID)
			if kimiPeerConflicts(kimiDirectory(cwd), otherProcess, otherCWD, otherErr) {
				code = "ambiguous_session"
				break
			}
		}
	}
	if code != "" && agent.ConversationReason == code && agent.SessionID == "" {
		return true
	}
	if code == "" && agent.ConversationReason != "ambiguous_session" && s.conversationM.KimiSelectionCurrent(conversationScope(agent)) {
		return true
	}
	if s.poller != nil {
		s.poller.Wake()
	}
	return false
}

func kimiDirectory(cwd string) string {
	return strings.TrimRight(strings.ReplaceAll(cwd, "\\", "/"), "/")
}

func kimiDirectoryForAgent(agent *coordinator.AgentState) string {
	project := projectContextForAgent(agent)
	cwd := project.ForegroundCWD
	if cwd == "" {
		cwd = project.CWD
	}
	return kimiDirectory(cwd)
}

func (s *Server) resolveKimiSessions(agents []*coordinator.AgentState) {
	for _, agent := range agents {
		if !conversation.IsKimi(agent.Agent) {
			continue
		}
		reported := strings.TrimSpace(agent.Session)
		agent.ConversationInferred = reported == "" && (agent.SessionKind == "" || agent.SessionKind == "id")
		agent.ConversationInferredOwner = ""
		if agent.ConversationInferred && strings.HasPrefix(agent.ConversationProcess, "verified:") {
			owner, _ := json.Marshal([]string{s.startedAt.Format(time.RFC3339Nano), agent.PaneID, agent.TerminalID, agent.TabID, agent.WorkspaceID, agent.Cwd, agent.ConversationProcess})
			agent.ConversationInferredOwner = fmt.Sprintf("%x", sha256.Sum256(owner))
		}
		if agent.ConversationReason != "" {
			continue
		}
		if agent.SessionKind != "" && agent.SessionKind != "id" {
			agent.ConversationReason = "invalid_session"
			continue
		}
		if agent.ConversationInferred {
			for _, other := range agents {
				if other == agent || !conversation.IsKimi(other.Agent) {
					continue
				}
				if kimiPeerConflicts(kimiDirectoryForAgent(agent), other.ConversationProcess, other.ForegroundCwd, nil) {
					agent.ConversationReason = "ambiguous_session"
					break
				}
			}
			if agent.ConversationReason != "" {
				continue
			}
		}
		id, location, code := s.conversationM.SelectKimiSession(projectContextForAgent(agent), reported)
		agent.ConversationSource = ""
		if agent.ConversationInferred && code == "" {
			agent.ConversationSource = conversation.KimiSourceIdentity(location)
		}
		agent.ConversationReason = code
		if code == "" {
			agent.Session = id
		}
	}
}
