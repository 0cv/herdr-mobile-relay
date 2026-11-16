package app

import (
	"context"
	"errors"

	"github.com/0cv/herdr-mobile-relay/internal/conversation"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
)

func (s *Server) conversationContentCurrent(ctx context.Context, agent *coordinator.AgentState) bool {
	current, exists := s.state.Agent(agent.PaneID)
	return exists && current.Generation == agent.Generation && current.TerminalID == agent.TerminalID && current.ServerSessionID == agent.ServerSessionID && sameConversationTuple(agent, current) && s.kimiSelectionCurrent(ctx, agent)
}

func (s *Server) readAgentConversation(ctx context.Context, agent *coordinator.AgentState, limit int) ([]conversation.Entry, error) {
	if agent == nil || !s.conversationContentCurrent(ctx, agent) {
		return nil, errors.New("conversation source changed")
	}
	var entries []conversation.Entry
	var err error
	if conversation.IsKimi(agent.Agent) && agent.ConversationInferred {
		entries, err = s.conversationM.ReadKimiSource(ctx, conversationScope(agent), limit)
	} else {
		entries, err = s.conversationM.ReadLatest(agent.Agent, projectContextForAgent(agent), agent.SessionID, limit)
	}
	if s.conversationHistoryReadObserver != nil {
		s.conversationHistoryReadObserver()
	}
	if err != nil {
		return nil, err
	}
	if !s.conversationContentCurrent(ctx, agent) {
		return nil, errors.New("conversation source changed")
	}
	return entries, nil
}
