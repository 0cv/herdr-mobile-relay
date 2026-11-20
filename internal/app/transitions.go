package app

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/conversation"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/push"
	"github.com/0cv/herdr-mobile-relay/internal/question"
)

func (s *Server) publishAgentPush(
	ctx context.Context,
	agent *coordinator.AgentState,
	eventID string,
	revision int64,
	category push.Category,
	preview push.PreviewMode,
) {
	if s.pushM == nil || agent == nil || agent.TerminalID == "" || eventID == "" {
		return
	}
	key := push.PushEventKey{
		ServerSessionID:     protocol.PrimaryServerSessionID,
		PaneID:              agent.PaneID,
		TerminalID:          agent.TerminalID,
		AgentSessionID:      agent.SessionID,
		Generation:          agent.Generation,
		EventID:             eventID,
		InteractionRevision: revision,
		Category:            category,
	}
	if err := s.pushM.ResolvePaneID(ctx, agent.PaneID, eventID); err != nil {
		s.logger.Warn("push notification retraction failed", "pane_id", agent.PaneID, "error", err)
	}
	if _, err := s.pushM.Publish(ctx, push.PublishRequest{
		Key: key, Preview: preview, ExpiresAt: time.Now().UTC().Add(5 * time.Minute),
	}); err != nil {
		s.logger.Warn("push notification queueing failed", "pane_id", agent.PaneID, "error", err)
	}
}

func (s *Server) handleTransition(
	parent context.Context,
	paneID, agent, project, status string,
	revision, transitionAt int64,
) {
	switch status {
	case "working":
		s.transitionWorking(parent, paneID, agent, project, revision, transitionAt)
	case "blocked":
		s.transitionBlocked(parent, paneID, agent, project, revision, transitionAt)
	default:
		s.transitionFinished(parent, paneID, agent, project, status, revision, transitionAt)
	}
}

func (s *Server) transitionWorking(
	parent context.Context,
	paneID, agent, project string,
	revision, transitionAt int64,
) {
	var session string
	if agentState, ok := s.state.Agent(paneID); ok && s.state.TransitionCurrent(paneID, "working", revision) {
		session = agentState.Session
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()

	if s.pushM != nil {
		if err := s.pushM.ResolvePaneID(ctx, paneID, ""); err != nil {
			s.logger.Warn("push notification retraction failed", "pane_id", paneID, "error", err)
		}
	}
	summary := agent + " started working"
	if agent == "" {
		summary = "Agent started working"
	}
	if s.dispatcher != nil {
		s.dispatcher.RecordTransitionActivity(coordinator.TransitionActivity{
			Kind:           "working",
			Status:         "working",
			Summary:        summary,
			PaneID:         paneID,
			ExpectedStatus: "working",
			Revision:       revision,
			Details:        map[string]any{"transition": "working", "transition_at": transitionAt},
			Agent:          agent,
			Project:        project,
			Host:           s.hostname,
			Session:        session,
		})
	}
}

func (s *Server) transitionBlocked(
	parent context.Context,
	paneID, agent, project string,
	revision, transitionAt int64,
) {
	agentState, blockedContentRevision, classifiedAttentionRevision, current := s.state.BlockedTransitionAgent(paneID, revision)
	if !current {
		return
	}
	session := agentState.Session
	blockedEventID := agentState.BlockedEventID
	paneGeneration := agentState.Generation
	transitionCurrent := func() bool {
		return s.state.BlockedTransitionCurrent(paneID, blockedEventID, paneGeneration)
	}
	if !transitionCurrent() {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()

	if !agentState.StrictAttention {
		if s.transitionEnrich != nil {
			s.transitionEnrich(ctx, agentState)
		} else {
			s.enrichBlockedTransition(ctx, agentState)
		}
		persisted, ok := s.state.CommitAttentionClassification(
			paneID, blockedEventID, paneGeneration, blockedContentRevision, classificationFromAgent(agentState),
		)
		if !ok || !transitionCurrent() {
			return
		}
		agentState = persisted
		classifiedAttentionRevision = s.state.AttentionRevision(paneID)
	}
	classifiedCurrent := func() bool {
		return s.state.AttentionTransitionCurrent(
			paneID,
			blockedEventID,
			paneGeneration,
			string(agentState.AttentionKind),
			classifiedAttentionRevision,
		)
	}
	if !classifiedCurrent() {
		return
	}
	if agentState.AttentionKind == question.AttentionChat {
		s.broadcastBlockedAttention(agentState)
		s.handleChatCompletion(ctx, agentState)
		return
	}

	eventID := agentState.BlockedEventID
	command := agentState.Command
	activityKind := "blocked"
	if command == "" {
		switch agentState.AttentionKind {
		case question.AttentionQuestion:
			command = "Agent needs an answer"
		case question.AttentionUnknown:
			command = "Agent needs inspection"
		default:
			command = "Agent needs approval"
		}
	}
	if agentState.AttentionKind == question.AttentionQuestion {
		activityKind = "question"
	}
	if s.dispatcher != nil && !s.dispatcher.RecordTransitionActivity(coordinator.TransitionActivity{
		Kind:           activityKind,
		Status:         "attention",
		Summary:        command,
		PaneID:         paneID,
		ExpectedStatus: "blocked",
		Revision:       agentState.StateRevision,
		Details: map[string]any{
			"event_id":       eventID,
			"attention_kind": agentState.AttentionKind,
			"transition_at":  transitionAt,
		},
		Agent:   agent,
		Project: project,
		Host:    s.hostname,
		Session: session,
		Extract: agentState.Prompt,
		BlockedIdentity: &coordinator.BlockedTransitionIdentity{
			EventID:           blockedEventID,
			Generation:        paneGeneration,
			AttentionKind:     string(agentState.AttentionKind),
			AttentionRevision: classifiedAttentionRevision,
		},
	}) {
		return
	}
	if !classifiedCurrent() {
		return
	}
	category := push.CategoryAttention
	preview := push.PreviewHidden
	if agentState.AttentionKind == question.AttentionQuestion {
		category = push.CategoryQuestion
		preview = push.PreviewQuestion
	}
	if s.pushM != nil {
		s.publishAgentPush(ctx, agentState, eventID, classifiedAttentionRevision, category, preview)
	}
	if !classifiedCurrent() {
		return
	}
	s.broadcastBlockedAttention(agentState)
}

func (s *Server) transitionFinished(
	parent context.Context,
	paneID, agent, project, status string,
	revision, transitionAt int64,
) {
	agentState, agentExists := s.state.Agent(paneID)
	var session string
	var sessionID string
	conversationAgent := agent
	var conversationCwd string
	conversationProject := conversation.ProjectContext{}
	if agentExists {
		session = agentState.Session
		sessionID = agentState.SessionID
		conversationAgent = agentState.Agent
		conversationCwd = agentState.Cwd
		conversationProject = projectContextForAgent(agentState)
	}
	transitionCurrent := func() bool {
		return s.state.CompletionCurrent(paneID, revision)
	}
	if !transitionCurrent() {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()

	if !s.state.RegisterFinishedNotificationForTransition(paneID, revision) {
		return
	}
	extract := s.captureFinishedPane(ctx, paneID, conversationAgent, conversationCwd, sessionID, conversationProject)
	currentAgent, currentExists := s.state.Agent(paneID)
	if !transitionCurrent() || agentExists != currentExists ||
		(agentExists && !sameConversationTuple(agentState, currentAgent)) {
		return
	}
	s.publishFinished(ctx, finishedNotice{
		paneID:         paneID,
		agent:          agent,
		project:        project,
		session:        session,
		expectedStatus: status,
		revision:       revision,
		details:        map[string]any{"transition_at": transitionAt},
		extract:        extract,
		pushAgent:      currentAgent,
	}, transitionCurrent)
}

type finishedNotice struct {
	paneID         string
	agent          string
	project        string
	session        string
	expectedStatus string
	revision       int64
	details        map[string]any
	extract        string
	pushAgent      *coordinator.AgentState
}

func (s *Server) publishFinished(ctx context.Context, notice finishedNotice, current func() bool) {
	eventID := fmt.Sprintf("finished-%d-%s", time.Now().UnixNano(), notice.paneID)
	notice.details["event_id"] = eventID
	summary := notice.agent + " completed"
	if notice.agent == "" {
		summary = "Agent completed"
	}
	if s.dispatcher != nil && !s.dispatcher.RecordTransitionActivity(coordinator.TransitionActivity{
		Kind:           "finished",
		Status:         "completed",
		Summary:        summary,
		PaneID:         notice.paneID,
		ExpectedStatus: notice.expectedStatus,
		Revision:       notice.revision,
		Details:        notice.details,
		Agent:          notice.agent,
		Project:        notice.project,
		Host:           s.hostname,
		Session:        notice.session,
		Extract:        notice.extract,
	}) {
		return
	}
	if !current() {
		return
	}
	if s.pushM == nil {
		return
	}
	if notice.pushAgent != nil {
		s.publishAgentPush(ctx, notice.pushAgent, eventID, notice.revision, push.CategoryFinished, push.PreviewBrief)
		return
	}
	if err := s.pushM.ResolvePaneID(ctx, notice.paneID, ""); err != nil {
		s.logger.Warn("push notification retraction failed", "pane_id", notice.paneID, "error", err)
	}
}

const (
	blockedClassificationAttempts   = 4
	blockedClassificationRetryDelay = 100 * time.Millisecond
)

func (s *Server) enrichBlockedTransition(ctx context.Context, agent *coordinator.AgentState) {
	if agent == nil {
		return
	}
	var content string
	classification, err := classifyBlockedTransition(ctx, agent.Agent, func(readCtx context.Context) (string, error) {
		attemptCtx, cancel := context.WithTimeout(readCtx, 3*time.Second)
		defer cancel()
		read, readErr := s.herdrC.ReadPane(attemptCtx, agent.PaneID, 80, "ansi")
		content = string(read.Content)
		return content, readErr
	})
	if err != nil {
		s.setNativeAttention(agent, unknownPaneClassification(), "")
		return
	}
	s.setNativeAttention(agent, classification, content)
}

func (s *Server) setNativeAttention(agent *coordinator.AgentState, classification question.Classification, content string) {
	if s.cfg.IdleQuestionProbe {
		live, _ := question.ClassifyLive(content, agent.Agent)
		agent.LiveIdentity = ""
		if classification.Kind == question.AttentionQuestion && live.Interaction != nil &&
			classification.Interaction != nil && classification.Interaction.ID == live.Interaction.ID ||
			classification.Kind == question.AttentionApproval && live.Kind == question.AttentionApproval &&
				classification.Command == live.Command && slices.Equal(classification.Options, live.Options) {
			agent.LiveIdentity = coordinator.LiveIdentity(live)
		}
	}
	setAgentAttention(s.state, agent, classification)
}

func unknownPaneClassification() question.Classification {
	return question.Classification{
		Kind:   question.AttentionUnknown,
		Prompt: "Agent needs inspection",
	}
}

func classifyBlockedTransition(
	ctx context.Context,
	agent string,
	read func(context.Context) (string, error),
) (question.Classification, error) {
	classification := unknownPaneClassification()
	for attempt := range blockedClassificationAttempts {
		content, err := read(ctx)
		if err != nil {
			return classification, err
		}
		classification = question.Classify(content, agent)
		if classification.Kind != question.AttentionUnknown ||
			attempt+1 == blockedClassificationAttempts {
			return classification, nil
		}
		timer := time.NewTimer(blockedClassificationRetryDelay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return classification, ctx.Err()
		case <-timer.C:
		}
	}
	return classification, nil
}

func setAgentAttention(
	state *coordinator.State,
	agent *coordinator.AgentState,
	classification question.Classification,
) {
	if classification.Kind == "" {
		classification.Kind = question.AttentionUnknown
	}
	agent.AttentionKind = classification.Kind
	agent.Prompt = classification.Prompt
	agent.Command = classification.Command
	agent.Options = nil
	agent.ApprovalFingerprint = ""
	agent.Interaction = nil
	agent.InteractionID = ""
	switch classification.Kind {
	case question.AttentionApproval:
		agent.Options = append([]string(nil), classification.Options...)
		agent.ApprovalFingerprint = question.ApprovalFingerprint(classification)
	case question.AttentionQuestion:
		agent.Interaction = classification.Interaction
		if agent.Interaction != nil {
			agent.InteractionID = agent.Interaction.ID
			if state != nil {
				if text := strings.TrimSpace(agent.Interaction.Other.Text); text != "" {
					state.RecordCustomAnswer(agent.PaneID, agent.Interaction.Question, text)
				}
				question.FillCustomAnswers(agent.Interaction, state.CustomAnswers(agent.PaneID))
			}
		}
	}
}

func classificationFromAgent(agent *coordinator.AgentState) question.Classification {
	if agent == nil {
		return question.Classification{Kind: question.AttentionUnknown}
	}
	return question.Classification{
		Kind:             agent.AttentionKind,
		Prompt:           agent.Prompt,
		Command:          agent.Command,
		Options:          append([]string(nil), agent.Options...),
		ApprovalIdentity: agent.ApprovalFingerprint,
		LiveIdentity:     agent.LiveIdentity,
		Interaction:      agent.Interaction,
	}
}

func (s *Server) broadcastBlockedAttention(agent *coordinator.AgentState) {
	if agent == nil || s.transitionBroadcast == nil {
		return
	}
	message := map[string]any{
		"type":                 "blocked",
		"pane_id":              agent.PaneID,
		"raw_pane_id":          agent.RawPaneID,
		"terminal_id":          agent.TerminalID,
		"tab_id":               agent.TabID,
		"tab_label":            agent.TabLabel,
		"tab_number":           agent.TabNumber,
		"workspace_id":         agent.WorkspaceID,
		"agent":                agent.Agent,
		"name":                 agent.Name,
		"status":               "blocked",
		"cwd":                  agent.Cwd,
		"project":              agent.Project,
		"host":                 agent.Host,
		"session":              agent.Session,
		"session_name":         agent.SessionName,
		"server_session_id":    protocol.PrimaryServerSessionID,
		"generation":           s.state.Generation(agent.PaneID),
		"agent_session_id":     agent.SessionID,
		"updated_at":           agent.UpdatedAt,
		"event_id":             agent.BlockedEventID,
		"attention_kind":       agent.AttentionKind,
		"prompt":               agent.Prompt,
		"command":              agent.Command,
		"options":              agent.Options,
		"approval_fingerprint": agent.ApprovalFingerprint,
		"interaction":          agent.Interaction,
		"interaction_id":       agent.InteractionID,
		"pane_revision":        agent.StateRevision,
	}
	if s.transitionBroadcast == s.hub {
		s.commitAgent(agent, message)
		return
	}
	s.transitionBroadcast.Broadcast(message)
}

func (s *Server) handleChatCompletion(
	ctx context.Context,
	agent *coordinator.AgentState,
) {
	if agent == nil {
		return
	}
	revision := agent.StateRevision
	current := func() bool {
		return s.state.CompletionCurrent(agent.PaneID, revision)
	}
	if !current() ||
		!s.state.RegisterFinishedNotificationForTransition(agent.PaneID, revision) {
		return
	}
	s.publishFinished(ctx, finishedNotice{
		paneID:         agent.PaneID,
		agent:          agent.Agent,
		project:        agent.Project,
		session:        agent.Session,
		expectedStatus: "blocked",
		revision:       revision,
		details:        map[string]any{"attention_kind": agent.AttentionKind},
		extract:        agent.Prompt,
		pushAgent:      agent,
	}, current)
}
