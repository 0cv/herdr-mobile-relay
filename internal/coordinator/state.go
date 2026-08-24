package coordinator

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/question"
)

type AgentState struct {
	PaneID                       string                 `json:"pane_id"`
	RawPaneID                    string                 `json:"raw_pane_id"`
	TerminalID                   string                 `json:"terminal_id"`
	TabID                        string                 `json:"tab_id"`
	TabLabel                     string                 `json:"tab_label"`
	TabNumber                    int                    `json:"tab_number"`
	TabOrder                     int                    `json:"tab_order,omitempty"`
	WorkspaceID                  string                 `json:"workspace_id"`
	Agent                        string                 `json:"agent"`
	Name                         string                 `json:"name"`
	Status                       string                 `json:"status"`
	Focused                      bool                   `json:"_focused"`
	Cwd                          string                 `json:"cwd"`
	Project                      string                 `json:"project"`
	Host                         string                 `json:"host"`
	Session                      string                 `json:"session"`
	SessionName                  string                 `json:"session_name"`
	UpdatedAt                    int64                  `json:"updated_at"`
	LastActiveAt                 int64                  `json:"last_active_at,omitempty"`
	LastSeenAt                   int64                  `json:"last_seen_at,omitempty"`
	ActivitySeq                  int64                  `json:"activity_seq,omitempty"`
	BlockedEventID               string                 `json:"event_id,omitempty"`
	AttentionKind                question.AttentionKind `json:"attention_kind,omitempty"`
	Prompt                       string                 `json:"prompt,omitempty"`
	Command                      string                 `json:"command,omitempty"`
	Options                      []string               `json:"options,omitempty"`
	Interaction                  *question.Interaction  `json:"interaction,omitempty"`
	QuestionLayout               bool                   `json:"question_layout,omitempty"`
	InteractionID                string                 `json:"-"`
	SessionID                    string                 `json:"-"`
	ConversationHistoryAvailable bool                   `json:"conversation_history_available,omitempty"`
	PaneRevision                 int                    `json:"-"`
	StateRevision                int64                  `json:"pane_revision,omitempty"`
	ScrollMaxOffset              int                    `json:"-"`
	ForegroundCwd                string                 `json:"-"`
}

type TransitionCallback func(paneID, agent, project, status string, revision int64)

type State struct {
	mu                 sync.RWMutex
	agents             map[string]*AgentState
	workspaces         []herdr.Workspace
	revision           map[string]int64
	contentRev         map[string]int64
	attentionRev       map[string]int64
	prevStatus         map[string]string
	topologyGen        int64
	revCounter         int64
	unseenDone         map[string]bool
	ackDone            map[string]bool
	finishedNotif      map[string]bool
	completionRev      map[string]int64
	generation         map[string]int64
	triage             map[string]triageRecord
	triagePath         string
	inventoryReady     bool
	inventoryErrorCode string
	inventoryMessage   string
	lastAttemptAt      time.Time
	lastSuccessAt      time.Time
	logger             *slog.Logger
	onTransition       TransitionCallback
	pendingEvents      map[string]pendingEvent
	topologyRetries    uint64
	blockedEventSeq    uint64
	customAnswers      map[string]map[string]string
}

type pendingEvent struct {
	tabID       string
	workspaceID string
	status      string
	updatedAt   int64
	expiresAt   time.Time
}

type PollToken struct {
	BaseRevision       int64
	TopologyGeneration int64
}

func NewState(logger *slog.Logger) *State {
	return &State{
		agents:        make(map[string]*AgentState),
		revision:      make(map[string]int64),
		contentRev:    make(map[string]int64),
		attentionRev:  make(map[string]int64),
		prevStatus:    make(map[string]string),
		unseenDone:    make(map[string]bool),
		ackDone:       make(map[string]bool),
		finishedNotif: make(map[string]bool),
		completionRev: make(map[string]int64),
		generation:    make(map[string]int64),
		pendingEvents: make(map[string]pendingEvent),
		logger:        logger,
		triage:        make(map[string]triageRecord),
		customAnswers: make(map[string]map[string]string),
	}
}

// RecordCustomAnswer remembers the free text typed for a question so review
// summaries can show it instead of the terminal's placeholder. Memory is
// process-local; a relay restart falls back to the placeholder.
func (s *State) RecordCustomAnswer(paneID, questionText, text string) {
	key := question.SummaryKey(questionText)
	text = strings.TrimSpace(text)
	if paneID == "" || key == "" || text == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	answers := s.customAnswers[paneID]
	if answers == nil {
		answers = make(map[string]string)
		s.customAnswers[paneID] = answers
	}
	if _, exists := answers[key]; !exists && len(answers) >= 32 {
		return
	}
	answers[key] = text
}

// CustomAnswers returns a copy of the recorded free-text answers for a pane.
func (s *State) CustomAnswers(paneID string) map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	answers := s.customAnswers[paneID]
	if len(answers) == 0 {
		return nil
	}
	copied := make(map[string]string, len(answers))
	for key, value := range answers {
		copied[key] = value
	}
	return copied
}
func (s *State) SetOnTransition(fn TransitionCallback) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onTransition = fn
}

var attentionStatuses = map[string]bool{
	"working": true,
	"blocked": true,
}

var doneStatuses = map[string]bool{
	"done":      true,
	"complete":  true,
	"completed": true,
	"finished":  true,
	"success":   true,
	"succeeded": true,
	"unread":    true,
}

// RevisionCounter returns the current global revision counter. The poller
// captures this before starting a poll; CommitInventory uses it to determine
// which panes received events during the poll's lifetime (§10.3).
func (s *State) RevisionCounter() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.revCounter
}

func (s *State) BeginPoll() PollToken {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return PollToken{BaseRevision: s.revCounter, TopologyGeneration: s.topologyGen}
}

func (s *State) InventoryReady() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.inventoryReady
}

func (s *State) MarkInventoryFailure(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastAttemptAt = time.Now().UTC()
	if err == nil {
		s.inventoryReady = false
		s.inventoryErrorCode = ""
		s.inventoryMessage = ""
		return
	}
	s.inventoryReady = false
	var cliErr *herdr.CLIError
	if errors.As(err, &cliErr) && cliErr.Code == "server_not_running" {
		s.inventoryErrorCode = "server_not_running"
		s.inventoryMessage = "Herdr is not running on this computer. Start it with `herdr`."
		return
	}
	s.inventoryErrorCode = "command_failed"
	s.inventoryMessage = "Unable to read the current Herdr agent inventory."
}

func (s *State) MarkTopologyDegraded() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastAttemptAt = time.Now().UTC()
	s.inventoryReady = false
	s.inventoryErrorCode = "topology_churn"
	s.inventoryMessage = "Agent inventory is changing too quickly to produce a stable snapshot."
}

func (s *State) InventoryStatus() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state := "starting"
	if s.inventoryReady {
		state = "ready"
	} else if s.inventoryErrorCode != "" {
		state = "error"
	}
	var lastAttempt, lastSuccess int64
	if !s.lastAttemptAt.IsZero() {
		lastAttempt = s.lastAttemptAt.Unix()
	}
	if !s.lastSuccessAt.IsZero() {
		lastSuccess = s.lastSuccessAt.Unix()
	}
	return map[string]any{
		"state":           state,
		"error_code":      s.inventoryErrorCode,
		"message":         s.inventoryMessage,
		"last_attempt_at": lastAttempt,
		"last_success_at": lastSuccess,
		"stale":           state != "ready" && !s.lastSuccessAt.IsZero(),
	}
}

func (s *State) Generation(paneID string) int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.generation[paneID]
}

func (s *State) PaneSession(paneID string) (int64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, active := s.agents[paneID]
	return s.generation[paneID], active
}

func (s *State) PaneSessionCurrent(paneID string, generation uint64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, active := s.agents[paneID]
	return active && uint64(s.generation[paneID]) == generation
}

func (s *State) BumpGeneration(paneID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.generation[paneID]++
}

func (s *State) TransitionCurrent(paneID, status string, revision int64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	agent := s.agents[paneID]
	return agent != nil && agent.Status == status && s.revision[paneID] == revision
}

func (s *State) BlockedTransitionCurrent(paneID, eventID string, generation uint64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	agent := s.agents[paneID]
	return agent != nil &&
		agent.Status == "blocked" &&
		agent.BlockedEventID == eventID &&
		uint64(s.generation[paneID]) == generation
}

func (s *State) AttentionTransitionCurrent(
	paneID, eventID string,
	generation uint64,
	attentionKind string,
	attentionRevision int64,
) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	agent := s.agents[paneID]
	return agent != nil &&
		agent.Status == "blocked" &&
		agent.BlockedEventID == eventID &&
		uint64(s.generation[paneID]) == generation &&
		string(agent.AttentionKind) == attentionKind &&
		s.attentionRev[paneID] == attentionRevision
}

func (s *State) CompletionCurrent(paneID string, revision int64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	agent := s.agents[paneID]
	return agent != nil &&
		(!attentionStatuses[agent.Status] ||
			agent.Status == "blocked" && agent.AttentionKind == question.AttentionChat) &&
		s.completionRev[paneID] == revision
}

func (s *State) MarkTopologyChanged() {
	s.mu.Lock()
	s.topologyGen++
	s.mu.Unlock()
}

// CommitInventory reconciles a full inventory snapshot from herdr. baseRev is
// the revision counter captured at poll start. Per §10.3, if a pane's revision
// advanced past baseRev (an event landed mid-poll), the event's status is
// preserved. A poll that starts after the event has baseRev >= the event's
// revision, so the fresh poll wins cleanly.
func (s *State) CommitInventory(agents []*AgentState, baseRev int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commitInventoryLocked(agents, baseRev)
}

// CommitTopology applies an event topology snapshot without allowing its
// sampled agent status to overwrite the current status stream for an existing
// pane. Workspace and agent topology commit under the same lock, and a
// workspace change advances topologyGen so an older reconcile poll is rejected.
func (s *State) CommitTopology(
	agents []*AgentState,
	workspaces []herdr.Workspace,
	baseRev int64,
) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	workspaceChanged := s.commitWorkspacesLocked(workspaces)
	if workspaceChanged {
		s.topologyGen++
	}
	s.commitTopologyLocked(agents, baseRev)
	return workspaceChanged
}

func (s *State) commitTopologyLocked(agents []*AgentState, baseRev int64) {
	topology := make([]*AgentState, len(agents))
	for index, incoming := range agents {
		if incoming == nil {
			continue
		}
		existing, exists := s.agents[incoming.PaneID]
		if !exists || paneSessionReplaced(existing, incoming) {
			topology[index] = incoming
			continue
		}
		cp := *incoming
		cp.Status = existing.Status
		if existing.Status == "blocked" {
			copyBlockedDetails(&cp, existing)
		}
		topology[index] = &cp
	}
	s.commitInventoryLocked(topology, baseRev)
}

// CommitPoll validates its token and commits both inventories while holding one
// lock. No workspace event can land between the topology-generation check and
// the workspace replacement.
func (s *State) CommitPoll(
	agents []*AgentState,
	workspaces []herdr.Workspace,
	token PollToken,
) (workspaceChanged, committed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.topologyGen != token.TopologyGeneration {
		s.topologyRetries++
		return false, false
	}
	workspaceChanged = s.commitWorkspacesLocked(workspaces)
	s.commitInventoryLocked(agents, token.BaseRevision)
	return workspaceChanged, true
}

func (s *State) commitInventoryLocked(agents []*AgentState, baseRev int64) {
	initialSnapshot := !s.inventoryReady
	topologyChanged := len(agents) != len(s.agents)
	if !topologyChanged {
		for _, agent := range agents {
			existing, exists := s.agents[agent.PaneID]
			if !exists || paneSessionReplaced(existing, agent) {
				topologyChanged = true
				break
			}
		}
	}
	if topologyChanged {
		s.topologyGen++
	}
	s.revCounter++
	s.inventoryReady = true
	s.inventoryErrorCode = ""
	s.inventoryMessage = ""
	s.lastAttemptAt = time.Now().UTC()
	s.lastSuccessAt = s.lastAttemptAt

	seen := make(map[string]bool, len(agents))
	triageDirty := false
	for _, incoming := range agents {
		seen[incoming.PaneID] = true

		cp := *incoming
		existing, exists := s.agents[incoming.PaneID]
		replaced := exists && paneSessionReplaced(existing, incoming)
		if replaced {
			topologyChanged = true
			s.generation[incoming.PaneID]++
			delete(s.prevStatus, incoming.PaneID)
			delete(s.unseenDone, incoming.PaneID)
			delete(s.ackDone, incoming.PaneID)
			delete(s.finishedNotif, incoming.PaneID)
			delete(s.completionRev, incoming.PaneID)
			existing = nil
			exists = false
		}
		if exists {
			cp.LastActiveAt = existing.LastActiveAt
			cp.LastSeenAt = existing.LastSeenAt
		} else {
			s.applyTriageLocked(&cp)
		}
		if !exists && !replaced && !initialSnapshot && s.generation[incoming.PaneID] > 0 {
			// Disappearance already ended the previous epoch. Reappearance must
			// establish another one so work admitted during the absence cannot
			// target the replacement session.
			s.generation[incoming.PaneID]++
		}

		// If an event landed during this poll (revision advanced past baseRev),
		// preserve the event's authoritative status.
		if exists && s.revision[incoming.PaneID] > baseRev && incoming.Status != existing.Status {
			cp.Status = existing.Status
			if existing.Status == "blocked" {
				copyBlockedDetails(&cp, existing)
			}
		}

		pendingTimestamp := false
		if pending, ok := s.pendingEvents[incoming.PaneID]; ok {
			if time.Now().Before(pending.expiresAt) &&
				eventIdentityMatches(incoming, pending.tabID, pending.workspaceID) {
				cp.Status = pending.status
				cp.UpdatedAt = pending.updatedAt
				pendingTimestamp = true
			}
			delete(s.pendingEvents, incoming.PaneID)
		}

		if !exists {
			switch {
			case pendingTimestamp:
			case initialSnapshot:
				cp.UpdatedAt = 0
			default:
				cp.UpdatedAt = time.Now().UnixMilli()
			}
		} else if existing.Status == cp.Status && existing.Name == cp.Name && existing.Cwd == cp.Cwd && existing.Agent == cp.Agent &&
			existing.ActivitySeq == cp.ActivitySeq &&
			existing.PaneRevision == cp.PaneRevision && existing.ScrollMaxOffset == cp.ScrollMaxOffset && existing.ForegroundCwd == cp.ForegroundCwd {
			cp.UpdatedAt = existing.UpdatedAt
		} else {
			cp.UpdatedAt = time.Now().UnixMilli()
		}
		activityAdvanced := pendingTimestamp || (exists &&
			(existing.Status != cp.Status || existing.ActivitySeq != cp.ActivitySeq))
		if activityAdvanced && cp.UpdatedAt > cp.LastActiveAt {
			cp.LastActiveAt = cp.UpdatedAt
		}

		s.applyBlockedCycleLocked(&cp, existing)
		attentionChanged := !blockedDetailsEqual(existing, &cp)

		if !exists || existing.Status != cp.Status || existing.Name != cp.Name || existing.Cwd != cp.Cwd ||
			existing.Agent != cp.Agent || existing.ActivitySeq != cp.ActivitySeq ||
			attentionChanged {
			s.contentRev[incoming.PaneID]++
		}
		if attentionChanged {
			s.attentionRev[incoming.PaneID]++
		}

		prev := s.prevStatus[incoming.PaneID]
		previousAttention := question.AttentionKind("")
		if existing != nil {
			previousAttention = existing.AttentionKind
		}
		s.revision[incoming.PaneID] = s.revCounter
		s.agents[incoming.PaneID] = &cp
		s.prevStatus[incoming.PaneID] = cp.Status
		s.registerTransition(incoming.PaneID, prev, cp.Status, previousAttention)
		if !exists && (doneStatuses[cp.Status] || cp.Status == "idle") {
			if cp.LastActiveAt > cp.LastSeenAt {
				s.unseenDone[incoming.PaneID] = true
				delete(s.ackDone, incoming.PaneID)
			} else if doneStatuses[cp.Status] && (cp.LastActiveAt > 0 || cp.LastSeenAt > 0) {
				s.ackDone[incoming.PaneID] = true
			}
		}
		triageDirty = s.syncTriageLocked(&cp) || triageDirty
		if !preservesChatCompletion(prev, cp.Status, previousAttention) {
			s.syncAttentionCompletionLocked(incoming.PaneID, previousAttention, cp.AttentionKind)
		}
		if prev == "blocked" && cp.Status == "blocked" &&
			(previousAttention != cp.AttentionKind ||
				attentionChanged && cp.AttentionKind == question.AttentionApproval) &&
			s.onTransition != nil {
			s.onTransition(cp.PaneID, cp.Agent, cp.Project, cp.Status, s.revision[cp.PaneID])
		}
	}

	for id := range s.agents {
		if !seen[id] {
			s.generation[id]++
			delete(s.agents, id)
			delete(s.revision, id)
			delete(s.contentRev, id)
			delete(s.attentionRev, id)
			delete(s.prevStatus, id)
			delete(s.unseenDone, id)
			delete(s.ackDone, id)
			delete(s.finishedNotif, id)
			delete(s.completionRev, id)
			delete(s.customAnswers, id)
		}
	}
	now := time.Now()
	for paneID, pending := range s.pendingEvents {
		if !now.Before(pending.expiresAt) {
			delete(s.pendingEvents, paneID)
		}
	}
	if triageDirty {
		s.persistTriageLocked()
	}
}

func paneSessionReplaced(existing, incoming *AgentState) bool {
	if existing == nil || incoming == nil {
		return false
	}
	for _, identity := range [][2]string{
		{existing.RawPaneID, incoming.RawPaneID},
		{existing.TerminalID, incoming.TerminalID},
		{existing.TabID, incoming.TabID},
		{existing.WorkspaceID, incoming.WorkspaceID},
	} {
		if identity[0] != "" && identity[1] != "" && identity[0] != identity[1] {
			return true
		}
	}
	return false
}

// CommitEvent applies an authoritative status update from a UDP event. These
// take precedence over in-flight inventory polls (§10.3).
func (s *State) CommitEvent(paneID, status string, updatedAt int64) bool {
	return s.CommitEventForSession(paneID, "", "", status, updatedAt)
}

// CommitEventForSession applies an event only when any supplied stable
// tab/workspace identity matches the current pane session. Empty identity is
// accepted for compatibility with older event-hook senders.
func (s *State) CommitEventForSession(
	paneID, tabID, workspaceID, status string,
	updatedAt int64,
) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	agent, exists := s.agents[paneID]
	if !exists {
		if len(s.pendingEvents) >= 128 {
			var oldestID string
			var oldest time.Time
			for id, event := range s.pendingEvents {
				if oldestID == "" || event.expiresAt.Before(oldest) {
					oldestID, oldest = id, event.expiresAt
				}
			}
			delete(s.pendingEvents, oldestID)
		}
		s.pendingEvents[paneID] = pendingEvent{
			tabID:       tabID,
			workspaceID: workspaceID,
			status:      status,
			updatedAt:   updatedAt,
			expiresAt:   time.Now().Add(30 * time.Second),
		}
		return false
	}
	if !eventIdentityMatches(agent, tabID, workspaceID) {
		return false
	}
	s.revCounter++
	s.revision[paneID] = s.revCounter

	a := agent
	before := *a
	before.Options = append([]string(nil), a.Options...)
	prev := a.Status
	previousAttention := a.AttentionKind
	a.Status = status
	a.UpdatedAt = updatedAt
	if updatedAt > a.LastActiveAt {
		a.LastActiveAt = updatedAt
	} else if updatedAt <= 0 {
		a.LastActiveAt = time.Now().UnixMilli()
	}
	if prev != status {
		s.contentRev[paneID]++
	}
	if status == "blocked" {
		if prev != "blocked" || a.BlockedEventID == "" {
			clearBlockedDetails(a)
			a.BlockedEventID = s.newBlockedEventIDLocked()
			a.AttentionKind = question.AttentionUnknown
		}
	} else {
		clearBlockedDetails(a)
	}
	if !blockedDetailsEqual(&before, a) {
		s.attentionRev[paneID]++
	}
	s.prevStatus[paneID] = status
	s.registerTransition(paneID, prev, status, previousAttention)
	if !preservesChatCompletion(prev, status, previousAttention) {
		s.syncAttentionCompletionLocked(paneID, previousAttention, a.AttentionKind)
	}
	if s.syncTriageLocked(a) {
		s.persistTriageLocked()
	}
	return true
}

func eventIdentityMatches(agent *AgentState, tabID, workspaceID string) bool {
	if agent == nil {
		return false
	}
	if tabID != "" && agent.TabID != "" && tabID != agent.TabID {
		return false
	}
	if workspaceID != "" && agent.WorkspaceID != "" && workspaceID != agent.WorkspaceID {
		return false
	}
	return true
}

func (s *State) applyBlockedCycleLocked(agent, existing *AgentState) {
	if agent.Status != "blocked" {
		clearBlockedDetails(agent)
		return
	}
	if agent.AttentionKind == "" {
		agent.AttentionKind = question.AttentionUnknown
	}
	if agent.BlockedEventID != "" {
		return
	}
	if existing != nil && existing.Status == "blocked" && existing.BlockedEventID != "" {
		agent.BlockedEventID = existing.BlockedEventID
		return
	}
	agent.BlockedEventID = s.newBlockedEventIDLocked()
}

func (s *State) newBlockedEventIDLocked() string {
	var value [12]byte
	if _, err := rand.Read(value[:]); err == nil {
		return base64.RawURLEncoding.EncodeToString(value[:])
	}
	s.blockedEventSeq++
	return fmt.Sprintf("blocked-%d-%d", time.Now().UnixNano(), s.blockedEventSeq)
}

func clearBlockedDetails(agent *AgentState) {
	agent.BlockedEventID = ""
	agent.AttentionKind = ""
	agent.Prompt = ""
	agent.Command = ""
	agent.Options = nil
	agent.Interaction = nil
	agent.QuestionLayout = false
	agent.InteractionID = ""
}

func copyBlockedDetails(destination, source *AgentState) {
	destination.BlockedEventID = source.BlockedEventID
	destination.AttentionKind = source.AttentionKind
	destination.Prompt = source.Prompt
	destination.Command = source.Command
	destination.Options = append([]string(nil), source.Options...)
	destination.Interaction = source.Interaction
	destination.QuestionLayout = source.QuestionLayout
	destination.InteractionID = source.InteractionID
}

// registerTransition implements the once-per-cycle notification state machine.
// Blocked notifications fire only on actual transitions into "blocked" (§16.13).
// Completion (working/blocked → idle) marks the pane as unseen-done (§9.8).
func (s *State) registerTransition(
	paneID, prev, status string,
	previousAttention question.AttentionKind,
) {
	if attentionStatuses[status] {
		if prev != status {
			delete(s.unseenDone, paneID)
			delete(s.ackDone, paneID)
			delete(s.finishedNotif, paneID)
			delete(s.completionRev, paneID)
		}
		if status == "blocked" && prev != "blocked" && s.onTransition != nil {
			a := s.agents[paneID]
			agent, project := "", ""
			if a != nil {
				agent, project = a.Agent, a.Project
			}
			s.onTransition(paneID, agent, project, status, s.revision[paneID])
		}
		if status == "working" && prev != "working" && s.onTransition != nil {
			a := s.agents[paneID]
			agent, project := "", ""
			if a != nil {
				agent, project = a.Agent, a.Project
			}
			s.onTransition(paneID, agent, project, status, s.revision[paneID])
		}
		return
	}

	if doneStatuses[status] {
		if attentionStatuses[prev] {
			delete(s.ackDone, paneID)
			s.unseenDone[paneID] = true
			s.completionRev[paneID] = s.revision[paneID]
			if s.onTransition != nil {
				a := s.agents[paneID]
				agent, project := "", ""
				if a != nil {
					agent, project = a.Agent, a.Project
				}
				s.onTransition(paneID, agent, project, status, s.revision[paneID])
			}
		}
		return
	}

	// §9.8: working/blocked → idle is the common completion path for agents
	// that don't emit an explicit "done" status.
	if status == "idle" && attentionStatuses[prev] {
		if prev == "blocked" && previousAttention == question.AttentionChat {
			return
		}
		delete(s.ackDone, paneID)
		s.unseenDone[paneID] = true
		s.completionRev[paneID] = s.revision[paneID]
		if s.onTransition != nil {
			a := s.agents[paneID]
			agent, project := "", ""
			if a != nil {
				agent, project = a.Agent, a.Project
			}
			s.onTransition(paneID, agent, project, status, s.revision[paneID])
		}
	}
}

func (s *State) syncAttentionCompletionLocked(
	paneID string,
	previous, current question.AttentionKind,
) {
	if current == question.AttentionChat {
		if previous != question.AttentionChat {
			delete(s.finishedNotif, paneID)
			s.completionRev[paneID] = s.revision[paneID]
		}
		return
	}
	if previous == question.AttentionChat {
		delete(s.finishedNotif, paneID)
		delete(s.completionRev, paneID)
	}
}

func preservesChatCompletion(
	previousStatus, currentStatus string,
	previousAttention question.AttentionKind,
) bool {
	return previousStatus == "blocked" &&
		currentStatus == "idle" &&
		previousAttention == question.AttentionChat
}

// CommitAttentionClassification persists pane controls only while the blocked
// event and pane generation that were read remain current.
func (s *State) CommitAttentionClassification(
	paneID, eventID string,
	generation uint64,
	contentRevision int64,
	classification question.Classification,
) (*AgentState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	agent := s.agents[paneID]
	if agent == nil || agent.Status != "blocked" ||
		agent.BlockedEventID != eventID ||
		uint64(s.generation[paneID]) != generation ||
		s.contentRev[paneID] != contentRevision {
		return nil, false
	}
	if classification.Kind == "" {
		classification.Kind = question.AttentionUnknown
	}
	previous := agent.AttentionKind
	changed := applyAttentionClassification(agent, classification)
	if changed {
		s.revCounter++
		s.revision[paneID] = s.revCounter
		s.contentRev[paneID]++
		s.attentionRev[paneID]++
		s.syncAttentionCompletionLocked(paneID, previous, agent.AttentionKind)
	}
	copy := *agent
	copy.StateRevision = s.revision[paneID]
	return &copy, true
}

func applyAttentionClassification(
	agent *AgentState,
	classification question.Classification,
) bool {
	options := append([]string(nil), classification.Options...)
	nextInteraction := classification.Interaction
	nextQuestionLayout := classification.QuestionLayout
	nextInteractionID := ""
	if classification.Kind != question.AttentionApproval {
		options = nil
	}
	if classification.Kind != question.AttentionQuestion {
		nextInteraction = nil
		nextQuestionLayout = false
	}
	if nextInteraction != nil {
		nextInteractionID = nextInteraction.ID
	}
	changed := agent.AttentionKind != classification.Kind ||
		agent.Prompt != classification.Prompt ||
		agent.Command != classification.Command ||
		!stringSlicesEqual(agent.Options, options) ||
		!interactionsEqual(agent.Interaction, nextInteraction) ||
		agent.QuestionLayout != nextQuestionLayout ||
		agent.InteractionID != nextInteractionID
	agent.AttentionKind = classification.Kind
	agent.Prompt = classification.Prompt
	agent.Command = classification.Command
	agent.Options = options
	agent.Interaction = nextInteraction
	agent.QuestionLayout = nextQuestionLayout
	agent.InteractionID = nextInteractionID
	return changed
}

func stringSlicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func blockedDetailsEqual(left, right *AgentState) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.BlockedEventID == right.BlockedEventID &&
		left.AttentionKind == right.AttentionKind &&
		left.Prompt == right.Prompt &&
		left.Command == right.Command &&
		stringSlicesEqual(left.Options, right.Options) &&
		interactionsEqual(left.Interaction, right.Interaction) &&
		left.QuestionLayout == right.QuestionLayout &&
		left.InteractionID == right.InteractionID
}

func interactionsEqual(left, right *question.Interaction) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.ID == right.ID
}

func (s *State) AcknowledgePane(paneID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	agent, exists := s.agents[paneID]
	if !exists {
		return false
	}
	agent.LastSeenAt = maxInt64(time.Now().UnixMilli(), agent.LastActiveAt)
	if s.unseenDone[paneID] || doneStatuses[agent.Status] {
		delete(s.unseenDone, paneID)
		s.ackDone[paneID] = true
	}
	if s.syncTriageLocked(agent) {
		s.persistTriageLocked()
	}
	return true
}

func (s *State) DisplayedStatus(paneID string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	a, ok := s.agents[paneID]
	if !ok {
		return ""
	}
	if doneStatuses[a.Status] && s.ackDone[paneID] {
		return "idle"
	}
	if a.Status == "idle" && s.unseenDone[paneID] {
		return "done"
	}
	return a.Status
}

func (s *State) Snapshot() []*AgentState {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*AgentState, 0, len(s.agents))
	for _, a := range s.agents {
		cp := *a
		cp.StateRevision = s.revision[cp.PaneID]
		if doneStatuses[cp.Status] && s.ackDone[cp.PaneID] {
			cp.Status = "idle"
		}
		if cp.Status == "idle" && s.unseenDone[cp.PaneID] {
			cp.Status = "done"
		}
		result = append(result, &cp)
	}
	// s.agents is a map; without an explicit order every snapshot serializes
	// differently, which defeats broadcast dedupe and hands clients a
	// re-shuffled payload each poll.
	sort.Slice(result, func(i, j int) bool { return result[i].PaneID < result[j].PaneID })
	return result
}

func (s *State) CommitWorkspaces(workspaces []herdr.Workspace) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commitWorkspacesLocked(workspaces)
}

func (s *State) commitWorkspacesLocked(workspaces []herdr.Workspace) bool {
	merged := cloneWorkspaces(workspaces)
	previousByID := make(map[string]herdr.Workspace, len(s.workspaces))
	for _, workspace := range s.workspaces {
		previousByID[workspace.ID] = workspace
	}
	for index := range merged {
		if merged[index].Cwd == "" {
			merged[index].Cwd = previousByID[merged[index].ID].Cwd
		}
	}
	if workspacesEqual(s.workspaces, merged) {
		return false
	}
	s.workspaces = merged
	return true
}

func (s *State) Workspaces() []herdr.Workspace {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneWorkspaces(s.workspaces)
}

func (s *State) Workspace(workspaceID string) (herdr.Workspace, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, workspace := range s.workspaces {
		if workspace.ID == workspaceID {
			copy := workspace
			if workspace.Worktree != nil {
				worktree := *workspace.Worktree
				copy.Worktree = &worktree
			}
			return copy, true
		}
	}
	return herdr.Workspace{}, false
}

func cloneWorkspaces(workspaces []herdr.Workspace) []herdr.Workspace {
	result := append([]herdr.Workspace(nil), workspaces...)
	for index := range result {
		if result[index].Worktree != nil {
			worktree := *result[index].Worktree
			result[index].Worktree = &worktree
		}
	}
	return result
}

func workspacesEqual(left, right []herdr.Workspace) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].ID != right[index].ID || left[index].Number != right[index].Number ||
			left[index].Label != right[index].Label || left[index].Focused != right[index].Focused ||
			left[index].PaneCount != right[index].PaneCount || left[index].TabCount != right[index].TabCount ||
			left[index].ActiveTabID != right[index].ActiveTabID ||
			left[index].AgentStatus != right[index].AgentStatus || left[index].Cwd != right[index].Cwd {
			return false
		}
		if left[index].Worktree == nil || right[index].Worktree == nil {
			if left[index].Worktree != nil || right[index].Worktree != nil {
				return false
			}
			continue
		}
		if *left[index].Worktree != *right[index].Worktree {
			return false
		}
	}
	return true
}

func (s *State) Agent(paneID string) (*AgentState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	agent, ok := s.agents[paneID]
	if !ok {
		return nil, false
	}
	copy := *agent
	copy.StateRevision = s.revision[paneID]
	return &copy, true
}

func (s *State) TopologyGeneration() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.topologyGen
}

func (s *State) Revision(paneID string) int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.revision[paneID]
}

func (s *State) ContentRevision(paneID string) int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.contentRev[paneID]
}

func (s *State) AttentionRevision(paneID string) int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.attentionRev[paneID]
}

func (s *State) AgentCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.agents)
}

func (s *State) TopologyRetryCount() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.topologyRetries
}

func (s *State) RegisterFinishedNotification(paneID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.finishedNotif[paneID] {
		return false
	}
	s.finishedNotif[paneID] = true
	return true
}

func (s *State) RegisterFinishedNotificationForTransition(
	paneID string,
	_ string,
	revision int64,
) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	agent := s.agents[paneID]
	if agent == nil ||
		(attentionStatuses[agent.Status] &&
			!(agent.Status == "blocked" && agent.AttentionKind == question.AttentionChat)) ||
		s.completionRev[paneID] != revision || s.finishedNotif[paneID] {
		return false
	}
	s.finishedNotif[paneID] = true
	return true
}

func (s *State) LastUpdatedAt() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var latest int64
	for _, a := range s.agents {
		if a.UpdatedAt > latest {
			latest = a.UpdatedAt
		}
	}
	if latest == 0 {
		return time.Time{}
	}
	return time.UnixMilli(latest)
}
