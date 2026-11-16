package app

import (
	"context"
	"strings"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/activity"
	"github.com/0cv/herdr-mobile-relay/internal/conversation"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
)

func (s *Server) recentActivities(limit int) []activity.Entry {
	s.activityMu.RLock()
	if limit <= 0 || limit > len(s.activityView) {
		limit = len(s.activityView)
	}
	start := len(s.activityView) - limit
	entries := append([]activity.Entry(nil), s.activityView[start:]...)
	s.activityMu.RUnlock()
	return s.enrichActivityResponses(entries)
}

func (s *Server) enrichActivityResponses(entries []activity.Entry) []activity.Entry {
	if s.conversationM == nil || len(entries) == 0 {
		return entries
	}
	agents := make(map[string]*coordinator.AgentState)
	for _, agent := range s.state.Snapshot() {
		agents[agent.PaneID] = agent
	}
	pages := make(map[string][]conversation.Entry)
	for index := range entries {
		entry := entries[index]
		if strings.ToLower(strings.TrimSpace(entry.Kind)) != "finished" {
			continue
		}
		agentName := strings.TrimSpace(entry.Agent)
		sessionID := strings.TrimSpace(entry.Session)
		project := conversation.ProjectContext{}
		if current := agents[entry.PaneID]; current != nil {
			if strings.TrimSpace(current.Agent) != "" {
				agentName = current.Agent
			}
			if strings.TrimSpace(current.SessionID) != "" {
				sessionID = current.SessionID
				project = projectContextForAgent(current)
			}
		}
		if agentName == "" || sessionID == "" || !conversation.Supported(agentName) {
			continue
		}
		if conversation.IsKimi(agentName) && agents[entry.PaneID] == nil {
			continue
		}
		var conversationEntries []conversation.Entry
		if current := agents[entry.PaneID]; current != nil && conversation.IsKimi(current.Agent) {
			var err error
			conversationEntries, err = s.readAgentConversation(context.Background(), current, 200)
			if err != nil {
				continue
			}
		} else {
			cacheKey := agentName + "\x00" + project.CWD + "\x00" + project.ForegroundCWD + "\x00" + sessionID
			var loaded bool
			conversationEntries, loaded = pages[cacheKey]
			if !loaded {
				conversationEntries, _ = s.conversationM.ReadLatest(agentName, project, sessionID, 200)
				pages[cacheKey] = conversationEntries
			}
		}
		if response := conversationResponseAt(conversationEntries, int64(entry.Timestamp)); response != "" {
			entries[index].Extract = response
		}
	}
	return entries
}

func conversationResponseAt(entries []conversation.Entry, activityTimestamp int64) string {
	for index := len(entries) - 1; index >= 0; index-- {
		entry := entries[index]
		if entry.Role != "assistant" || strings.TrimSpace(entry.Text) == "" {
			continue
		}
		if activityTimestamp <= 0 {
			return entry.Text
		}
		timestamp, err := time.Parse(time.RFC3339Nano, entry.Timestamp)
		if err != nil || timestamp.UnixMilli() > activityTimestamp {
			continue
		}
		return entry.Text
	}
	return ""
}
