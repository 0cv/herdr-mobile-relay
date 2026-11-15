package app

import (
	"context"
	"strings"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/agentkind"
	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
	"github.com/0cv/herdr-mobile-relay/internal/conversation"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/history"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/question"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

func (s *Server) captureHistoryLoop(ctx context.Context) {
	ticker := time.NewTicker(history.CaptureInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, agent := range s.state.Snapshot() {
				if !isClaudeLike(agent.Agent) || (agent.Status != "working" && agent.Status != "blocked") {
					continue
				}
				s.scheduleHistoryCapture(ctx, agent.PaneID)
			}
		}
	}
}

func (s *Server) scheduleHistoryCapture(ctx context.Context, paneID string) {
	if s.historyTasks == nil {
		return
	}
	s.historyCaptureMu.Lock()
	if s.historyInflight[paneID] || time.Since(s.historyLast[paneID]) < history.CaptureInterval {
		s.historyCaptureMu.Unlock()
		return
	}
	s.historyInflight[paneID] = true
	s.historyLast[paneID] = time.Now()
	s.historyCaptureMu.Unlock()

	started := s.historyTasks.Start(func(taskCtx context.Context) {
		defer func() {
			s.historyCaptureMu.Lock()
			delete(s.historyInflight, paneID)
			s.historyCaptureMu.Unlock()
		}()
		readCtx, cancel := context.WithTimeout(taskCtx, 3*time.Second)
		defer cancel()
		read, err := s.herdrC.ReadPane(readCtx, paneID, history.MaxLines, "ansi")
		content := read.Content
		if err != nil || len(content) == 0 || question.LayoutHint(string(content)) {
			return
		}
		agent, ok := s.state.Agent(paneID)
		if !ok || !isClaudeLike(agent.Agent) {
			return
		}
		s.historyCaptureMu.Lock()
		defer s.historyCaptureMu.Unlock()
		if !s.historyActive[paneID] {
			return
		}
		s.historyM.Merge(paneID, string(content))
	})
	if started {
		return
	}
	s.historyCaptureMu.Lock()
	delete(s.historyInflight, paneID)
	s.historyCaptureMu.Unlock()
}

func (s *Server) syncHistoryPanes(agents []*coordinator.AgentState) {
	active := make(map[string]bool, len(agents))
	for _, agent := range agents {
		if isClaudeLike(agent.Agent) {
			active[agent.PaneID] = true
		}
	}

	s.historyCaptureMu.Lock()
	defer s.historyCaptureMu.Unlock()
	if !s.historyReconciled {
		s.historyM.Reconcile(active)
		s.historyReconciled = true
	}
	for paneID := range s.historyActive {
		if active[paneID] {
			continue
		}
		delete(s.historyActive, paneID)
		delete(s.historyLast, paneID)
		s.historyM.Discard(paneID)
	}
	for paneID := range active {
		s.historyActive[paneID] = true
	}
}

func (s *Server) handleConversationHistory(client *transport.ClientConn, inbound protocol.Inbound) {
	action := "get_conversation_history"
	reply := s.reply(client, inbound.RequestID, action, inbound.PaneID)
	agent, exists := s.state.Agent(inbound.PaneID)
	if !exists {
		reply.fail("Agent is unavailable")
		return
	}
	generation := s.state.Generation(inbound.PaneID)
	browser := s.conversationB
	if browser == nil || browser.Reader() != s.conversationM {
		s.logger.Warn("conversation browser is unavailable", "pane_id", inbound.PaneID)
		reply.fail("Conversation history could not be read")
		return
	}
	if !s.kimiSelectionCurrent(client.Context(), agent) {
		reply.fail("Kimi session changed")
		return
	}
	var page conversation.BrowsePage
	var historyErr error
	if inbound.Observe && supportsConversationActivity(agent.Agent) {
		page, historyErr = s.conversationFeedPage(client, inbound, agent)
	} else {
		page, historyErr = browser.ReadPage(client.Context(), conversation.BrowseRequest{
			Scope: conversationScope(agent), Cursor: inbound.Cursor, Limit: inbound.Limit, Retry: inbound.Retry,
		})
	}
	if historyErr != nil {
		s.logger.Warn("conversation history read failed", "pane_id", inbound.PaneID)
		reply.fail("Conversation history could not be read")
		return
	}
	if s.conversationHistoryReadObserver != nil {
		s.conversationHistoryReadObserver()
	}
	current, currentExists := s.state.Agent(inbound.PaneID)
	if !currentExists || s.state.Generation(inbound.PaneID) != generation ||
		!sameConversationTuple(agent, current) || !s.kimiSelectionCurrent(client.Context(), agent) ||
		s.authorizeDeviceAction(client, protocol.ActionMetadata{Operation: action, Class: protocol.ActionReadOnly}, "") != nil {
		reply.fail("Agent changed while conversation history was loading")
		return
	}
	reply.complete(page)
}

func (s *Server) latestConversationResponse(agent, sessionID string, project conversation.ProjectContext) string {
	if s.conversationM == nil || conversation.IsKimi(agent) || strings.TrimSpace(sessionID) == "" || !conversation.Supported(agent) {
		return ""
	}
	page, err := s.conversationM.ReadWithProject(agent, project, sessionID, "", 1)
	if err != nil || !page.Available || len(page.Entries) == 0 {
		return ""
	}
	entry := page.Entries[len(page.Entries)-1]
	if entry.Role != "assistant" || strings.TrimSpace(entry.Text) == "" {
		return ""
	}
	return entry.Text
}

func (s *Server) captureFinishedPane(ctx context.Context, paneID, agent, cwd, sessionID string, project conversation.ProjectContext) string {
	if conversation.IsKimi(agent) {
		current, exists := s.state.Agent(paneID)
		if !exists || current.Agent != agent || current.SessionID != sessionID || current.Cwd != cwd || s.conversationM == nil {
			return ""
		}
		page, err := s.readAgentConversation(ctx, current, 1)
		if err != nil || !page.Available {
			return ""
		}
		if len(page.Entries) > 0 {
			entry := page.Entries[len(page.Entries)-1]
			if entry.Role == "assistant" && strings.TrimSpace(entry.Text) != "" {
				return entry.Text
			}
		}
	}
	if response := s.latestConversationResponse(agent, sessionID, project); response != "" {
		return response
	}
	readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	read, err := s.herdrC.ReadPane(readCtx, paneID, history.MaxLines, "ansi")
	content := read.Content
	if err != nil || len(content) == 0 {
		return ""
	}
	raw := string(content)
	completionContent := raw
	if isClaudeLike(agent) && !question.LayoutHint(raw) {
		completionContent = s.historyM.Merge(paneID, raw)
	}
	if response := question.LatestCompletedResponse(completionContent); response != "" {
		return response
	}
	return question.PaneSummary(completionContent)
}

func locatedAgentDir(home, agent string, location conversation.Location) string {
	return agentroots.AgentDirForSession(home, agent, location.Path)
}

func sameConversationTuple(left, right *coordinator.AgentState) bool {
	if left == nil || right == nil {
		return false
	}
	leftProject := projectContextForAgent(left)
	rightProject := projectContextForAgent(right)
	return left.Agent == right.Agent &&
		left.Cwd == right.Cwd &&
		leftProject == rightProject &&
		left.SessionID == right.SessionID &&
		left.ConversationProcess == right.ConversationProcess &&
		left.ConversationSource == right.ConversationSource &&
		left.ConversationReason == right.ConversationReason &&
		left.ConversationInferred == right.ConversationInferred
}

func isClaudeLike(agent string) bool {
	kind := agentkind.Of(agent)
	return kind == agentkind.Claude || kind == agentkind.Qoder
}
