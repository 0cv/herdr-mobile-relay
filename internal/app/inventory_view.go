package app

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/0cv/herdr-mobile-relay/internal/activity"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/push"
	"github.com/0cv/herdr-mobile-relay/internal/question"
)

func (s *Server) reconcileRecoveredPush(ctx context.Context) {
	if s.pushM == nil {
		return
	}
	s.pushReconcileMu.Lock()
	defer s.pushReconcileMu.Unlock()
	if s.pushReconciled {
		return
	}
	if err := s.pushM.Reconcile(ctx, func(key push.PushEventKey) bool {
		if key.Category.Waiting() {
			return s.pushEventCurrent(key)
		}
		return key.Category == push.CategoryFinished &&
			s.pushTargetCurrent(key.Target()) &&
			s.state.CompletionCurrent(key.PaneID, key.InteractionRevision)
	}); err != nil {
		s.logger.Warn("recovered push queue reconciliation failed", "error", err)
		return
	}
	s.pushReconciled = true
}

func (s *Server) observeInventoryPublication(phase string) {
	if s.inventoryPublicationObserver != nil {
		s.inventoryPublicationObserver(phase)
	}
}

func inventoryStatusMessage(status map[string]any) map[string]any {
	return map[string]any{
		"type":            "inventory_status",
		"state":           status["state"],
		"error_code":      status["error_code"],
		"message":         status["message"],
		"last_attempt_at": status["last_attempt_at"],
		"last_success_at": status["last_success_at"],
		"stale":           status["stale"],
	}
}

func (s *Server) projectAgentResource(agent *coordinator.AgentState) {
	if agent == nil {
		return
	}
	agent.ServerSessionID = protocol.PrimaryServerSessionID
}

func (s *Server) projectAgentResources(agents []*coordinator.AgentState) {
	for _, agent := range agents {
		s.projectAgentResource(agent)
	}
}

type committedInventory struct {
	status     map[string]any
	agents     []*coordinator.AgentState
	workspaces []herdr.Workspace
}

func (inventory committedInventory) messages() []any {
	return []any{
		inventoryStatusMessage(inventory.status),
		map[string]any{"type": "agents", "agents": inventory.agents},
		map[string]any{"type": "workspaces", "workspaces": inventory.workspaces},
	}
}

// publishCurrentInventory is the sole authoritative inventory writer. Fresh
// state is selected inside the Hub registration barrier, while the expensive
// reconciliation work remains after that barrier has been released.
func (s *Server) setInventoryPublisher(ctx context.Context) {
	if s.poller == nil {
		return
	}
	s.poller.SetOnInventoryChange(func() error {
		return s.publishCurrentInventory(ctx)
	})
}

func (s *Server) publishCurrentInventory(ctx context.Context) error {
	var sideEffectAgents []*coordinator.AgentState
	var runAgentSideEffects bool
	batchErr := s.hub.BroadcastBatchPrepared(func() ([]any, func(), error) {
		fresh := s.state.InventorySnapshot()
		s.observeInventoryPublication("publication")
		freshAgents := cloneAgents(fresh.Agents)
		s.projectAgentResources(freshAgents)

		s.stateViewMu.RLock()
		previousStatus := cloneStringMap(s.inventoryView)
		previousAgents := cloneAgents(s.agentView)
		previousWorkspaces := cloneWorkspaces(s.workspaceView)
		s.stateViewMu.RUnlock()

		mergedAgents := mergeAgentSnapshot(previousAgents, freshAgents)
		statusChanged := inventoryStatusChanged(previousStatus, fresh.Status)
		readyRecovery := fresh.Status["state"] == "ready" && previousStatus["state"] != "ready"
		agentsChanged := !agentSnapshotsEqual(previousAgents, mergedAgents)
		workspacesChanged := !workspaceSnapshotsEqual(previousWorkspaces, fresh.Workspaces)
		sendAgents := agentsChanged || readyRecovery
		sendWorkspaces := workspacesChanged || readyRecovery
		if fresh.Status["state"] == "ready" && (agentsChanged || readyRecovery) {
			runAgentSideEffects = true
			sideEffectAgents = cloneAgents(mergedAgents)
		}

		messages := make([]any, 0, 3)
		if statusChanged {
			messages = append(messages, inventoryStatusMessage(fresh.Status))
		}
		if sendAgents {
			messages = append(messages, map[string]any{"type": "agents", "agents": cloneAgents(mergedAgents)})
		}
		if sendWorkspaces {
			messages = append(messages, map[string]any{"type": "workspaces", "workspaces": cloneWorkspaces(fresh.Workspaces)})
		}

		commit := func() {
			s.stateViewMu.Lock()
			// Timestamps are metadata, not a wire-change trigger. Refreshing the
			// cached status here still makes a later handshake authoritative.
			s.inventoryView = cloneStringMap(fresh.Status)
			if sendAgents {
				s.agentView = cloneAgents(mergedAgents)
			}
			if sendWorkspaces {
				s.workspaceView = cloneWorkspaces(fresh.Workspaces)
			}
			s.stateViewMu.Unlock()
		}
		return messages, commit, nil
	})

	// A pending explicit refresh is a request for completion, not a request for
	// an agent diff. Drain it after every publication attempt, including an
	// unchanged state and an encode failure.
	s.sendRequestedAgentRefreshes()
	if batchErr != nil {
		return batchErr
	}
	if runAgentSideEffects {
		s.reconcileRecoveredPush(ctx)
		s.syncHistoryPanes(sideEffectAgents)
		active := make(map[string]bool, len(sideEffectAgents))
		for _, agent := range sideEffectAgents {
			active[agent.PaneID] = true
			if historyCaptureEligible(agent) {
				s.scheduleHistoryCapture(ctx, agent.PaneID)
			}
		}
		if s.dispatcher != nil {
			s.dispatcher.PruneSlots(active)
		}
	}
	return nil
}

func inventoryStatusChanged(previous, current map[string]any) bool {
	for _, key := range []string{"state", "error_code", "message", "stale"} {
		if previous[key] != current[key] {
			return true
		}
	}
	return false
}

func agentSnapshotsEqual(left, right []*coordinator.AgentState) bool {
	left = cloneAgents(left)
	right = cloneAgents(right)
	for _, agents := range [][]*coordinator.AgentState{left, right} {
		for _, agent := range agents {
			agent.StateRevision = 0
		}
	}
	leftData, leftErr := json.Marshal(left)
	rightData, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftData, rightData)
}

func workspaceSnapshotsEqual(left, right []herdr.Workspace) bool {
	if len(left) == 0 && len(right) == 0 {
		return true
	}
	leftData, leftErr := json.Marshal(left)
	rightData, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftData, rightData)
}

func cloneWorkspaces(workspaces []herdr.Workspace) []herdr.Workspace {
	result := make([]herdr.Workspace, len(workspaces))
	copy(result, workspaces)
	for index := range result {
		if result[index].Worktree != nil {
			worktree := *result[index].Worktree
			result[index].Worktree = &worktree
		}
	}
	return result
}

func (s *Server) committedInventorySnapshot() committedInventory {
	s.stateViewMu.RLock()
	result := committedInventory{
		status:     cloneStringMap(s.inventoryView),
		agents:     cloneAgents(s.agentView),
		workspaces: cloneWorkspaces(s.workspaceView),
	}
	s.stateViewMu.RUnlock()
	s.projectAgentResources(result.agents)
	return result
}

func (s *Server) commitAgent(agent *coordinator.AgentState, message map[string]any) {
	paneID := agent.PaneID
	if paneID == "" {
		s.recordSafeError("agent update broadcast was malformed", nil)
		return
	}
	err := s.hub.BroadcastBatchPrepared(func() ([]any, func(), error) {
		current, exists := s.state.Agent(paneID)
		if !exists || current.ProbeTarget() != agent.ProbeTarget() || current.StateRevision > agent.StateRevision {
			return nil, nil, nil
		}
		s.stateViewMu.RLock()
		agents := cloneAgents(s.agentView)
		s.stateViewMu.RUnlock()
		found := false
		for _, view := range agents {
			if view.PaneID != paneID {
				continue
			}
			if view.StateRevision > agent.StateRevision {
				return nil, nil, nil
			}
			applyCommittedAgent(view, agent)
			found = true
			break
		}
		if !found {
			applyCommittedAgent(current, agent)
			agents = append(agents, current)
		}

		message["server_session_id"] = protocol.PrimaryServerSessionID
		message["generation"] = agent.Generation
		message["terminal_id"] = agent.TerminalID
		message["agent_session_id"] = agent.SessionID
		setAgentAttentionFields(message, agent)
		return []any{message}, func() {
			s.stateViewMu.Lock()
			s.agentView = agents
			s.stateViewMu.Unlock()
		}, nil
	})
	if err != nil {
		s.recordSafeError("agent update broadcast failed", err)
	}
}

func setAgentAttentionFields(message map[string]any, agent *coordinator.AgentState) {
	message["event_id"] = agent.BlockedEventID
	message["attention_kind"] = agent.AttentionKind
	message["prompt"] = agent.Prompt
	message["command"] = agent.Command
	message["options"] = agent.Options
	message["approval_fingerprint"] = agent.ApprovalFingerprint
	message["interaction"] = agent.Interaction
	message["interaction_id"] = agent.InteractionID
	if agent.ScreenAttention != "" {
		message["screen_attention"] = agent.ScreenAttention
	}
}

func (s *Server) broadcastCommitted(message any) {
	if update, ok := message.(coordinator.AgentUpdate); ok {
		s.commitAgent(update.Agent, update.Message)
		return
	}
	envelope, ok := message.(map[string]any)
	if !ok {
		s.hub.Broadcast(message)
		return
	}
	messageType, _ := envelope["type"].(string)
	switch messageType {
	case "activity":
		var entry activity.Entry
		data, err := json.Marshal(envelope["activity"])
		if err != nil {
			s.recordSafeError("activity broadcast was malformed", err)
			return
		}
		if err := json.Unmarshal(data, &entry); err != nil || entry.ID == "" {
			s.recordSafeError("activity broadcast was malformed", err)
			return
		}
		s.hub.BroadcastPrepared(message, func() {
			s.activityMu.Lock()
			s.activityView = append(s.activityView, entry)
			if len(s.activityView) > 500 {
				s.activityView = append([]activity.Entry(nil), s.activityView[len(s.activityView)-500:]...)
			}
			s.activityMu.Unlock()
		})
	case "activity_history":
		var entries []activity.Entry
		data, err := json.Marshal(envelope["activities"])
		if err != nil {
			s.recordSafeError("activity history broadcast was malformed", err)
			return
		}
		if err := json.Unmarshal(data, &entries); err != nil {
			s.recordSafeError("activity history broadcast was malformed", err)
			return
		}
		s.hub.BroadcastPrepared(message, func() {
			s.activityMu.Lock()
			s.activityView = append([]activity.Entry(nil), entries...)
			s.activityMu.Unlock()
		})
	default:
		s.hub.Broadcast(message)
	}
}

func cloneAgents(agents []*coordinator.AgentState) []*coordinator.AgentState {
	result := make([]*coordinator.AgentState, 0, len(agents))
	for _, agent := range agents {
		if agent == nil {
			continue
		}
		result = append(result, agent.Clone())
	}
	return result
}

func mergeAgentSnapshot(current, incoming []*coordinator.AgentState) []*coordinator.AgentState {
	merged := cloneAgents(incoming)
	currentByPane := make(map[string]*coordinator.AgentState, len(current))
	for _, agent := range current {
		currentByPane[agent.PaneID] = agent
	}
	for index, agent := range merged {
		if existing := currentByPane[agent.PaneID]; existing != nil &&
			agent.StateRevision < existing.StateRevision {
			merged[index] = existing.Clone()
		}
	}
	return merged
}

func cloneStringMap(values map[string]any) map[string]any {
	result := make(map[string]any, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func applyCommittedAgent(view, committed *coordinator.AgentState) {
	view.RawPaneID = committed.RawPaneID
	view.Status = committed.Status
	view.ScreenAttention = committed.ScreenAttention
	view.RawStatus = committed.RawStatus
	view.ProbeEnabled = committed.ProbeEnabled
	view.ProbePending = committed.ProbePending
	view.StrictAttention = committed.StrictAttention
	view.LiveIdentity = committed.LiveIdentity
	view.Generation = committed.Generation
	view.TerminalID = committed.TerminalID
	view.SessionID = committed.SessionID
	view.AgentSessionID = committed.SessionID
	view.Agent = committed.Agent
	view.TabID = committed.TabID
	view.TabLabel = committed.TabLabel
	view.TabNumber = committed.TabNumber
	view.WorkspaceID = committed.WorkspaceID
	view.Cwd = committed.Cwd
	view.Project = committed.Project
	view.Host = committed.Host
	view.Session = committed.Session
	view.SessionName = committed.SessionName
	view.UpdatedAt = committed.UpdatedAt
	view.BlockedEventID = committed.BlockedEventID
	view.AttentionKind = committed.AttentionKind
	view.Prompt = committed.Prompt
	view.Command = committed.Command
	view.Options = append([]string(nil), committed.Options...)
	view.ApprovalFingerprint = committed.ApprovalFingerprint
	view.Interaction = committed.Clone().Interaction
	view.InteractionID = committed.InteractionID
	view.StateRevision = committed.StateRevision
	if view.Status != "blocked" {
		view.AttentionKind = ""
		view.Prompt = ""
		view.Command = ""
		view.Options = nil
		view.ApprovalFingerprint = ""
		view.Interaction = nil
		return
	}
	if view.AttentionKind != question.AttentionApproval {
		view.Options = nil
		view.ApprovalFingerprint = ""
	}
	if view.AttentionKind != question.AttentionQuestion {
		view.Interaction = nil
	}
}
