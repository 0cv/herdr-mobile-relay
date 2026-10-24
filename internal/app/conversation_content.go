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

func (s *Server) readAgentConversation(ctx context.Context, agent *coordinator.AgentState, limit int) (conversation.Page, error) {
	if agent == nil || !s.conversationContentCurrent(ctx, agent) {
		return conversation.Page{}, errors.New("conversation source changed")
	}
	var page conversation.Page
	var err error
	if conversation.IsKimi(agent.Agent) && agent.ConversationInferred {
		page, err = s.conversationM.ReadKimiSource(ctx, conversationScope(agent), "", limit)
	} else {
		page, err = s.conversationM.ReadWithProject(agent.Agent, projectContextForAgent(agent), agent.SessionID, "", limit)
	}
	if s.conversationHistoryReadObserver != nil {
		s.conversationHistoryReadObserver()
	}
	if err != nil {
		return conversation.Page{}, err
	}
	if !s.conversationContentCurrent(ctx, agent) {
		return conversation.Page{}, errors.New("conversation source changed")
	}
	return page, nil
}
