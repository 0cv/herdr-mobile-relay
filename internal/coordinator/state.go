package coordinator

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/question"
)

type AgentState struct {
	PaneID          string                `json:"pane_id"`
	RawPaneID       string                `json:"raw_pane_id"`
	TerminalID      string                `json:"terminal_id"`
	TabID           string                `json:"tab_id"`
	TabLabel        string                `json:"tab_label"`
	TabNumber       int                   `json:"tab_number"`
	WorkspaceID     string                `json:"workspace_id"`
	Agent           string                `json:"agent"`
	Name            string                `json:"name"`
	Status          string                `json:"status"`
	Focused         bool                  `json:"_focused"`
	Cwd             string                `json:"cwd"`
	Project         string                `json:"project"`
	Host            string                `json:"host"`
	Session         string                `json:"session"`
	UpdatedAt       int64                 `json:"updated_at"`
	BlockedEventID  string                `json:"event_id,omitempty"`
	Prompt          string                `json:"prompt,omitempty"`
	Command         string                `json:"command,omitempty"`
	Options         []string              `json:"options,omitempty"`
	Interaction     *question.Interaction `json:"interaction,omitempty"`
	QuestionLayout  bool                  `json:"question_layout,omitempty"`
	InteractionID   string                `json:"-"`
	PaneRevision    int                   `json:"-"`
	ScrollMaxOffset int                   `json:"-"`
	ForegroundCwd   string                `json:"-"`
}

type TransitionCallback func(paneID, agent, project, status string, revision int64)

type State struct {
	mu                 sync.RWMutex
	agents             map[string]*AgentState
	revision           map[string]int64
	contentRev         map[string]int64
	prevStatus         map[string]string
	topologyGen        int64
	revCounter         int64
	unseenDone         map[string]bool
	ackDone            map[string]bool
	finishedNotif      map[string]bool
	generation         map[string]int64
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
}

type pendingEvent struct {
	status    string
	updatedAt int64
	expiresAt time.Time
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
		prevStatus:    make(map[string]string),
		unseenDone:    make(map[string]bool),
		ackDone:       make(map[string]bool),
		finishedNotif: make(map[string]bool),
		generation:    make(map[string]int64),
		pendingEvents: make(map[string]pendingEvent),
		logger:        logger,
	}
}

func (s *State) SetOnTransition(fn TransitionCallback) {
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
	s.inventoryReady = false
	s.inventoryErrorCode = "command_failed"
	s.inventoryMessage = "Unable to read the current Herdr agent inventory."
	if err == nil {
		s.inventoryErrorCode = ""
		s.inventoryMessage = ""
	}
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

func (s *State) CommitPoll(agents []*AgentState, token PollToken) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.topologyGen != token.TopologyGeneration {
		s.topologyRetries++
		return false
	}
	s.commitInventoryLocked(agents, token.BaseRevision)
	return true
}

func (s *State) commitInventoryLocked(agents []*AgentState, baseRev int64) {
	topologyChanged := len(agents) != len(s.agents)
	if !topologyChanged {
		for _, agent := range agents {
			if _, exists := s.agents[agent.PaneID]; !exists {
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
	for _, incoming := range agents {
		seen[incoming.PaneID] = true

		cp := *incoming
		existing, exists := s.agents[incoming.PaneID]

		// If an event landed during this poll (revision advanced past baseRev),
		// preserve the event's authoritative status.
		if exists && s.revision[incoming.PaneID] > baseRev && incoming.Status != existing.Status {
			cp.Status = existing.Status
		}

		if pending, ok := s.pendingEvents[incoming.PaneID]; ok {
			if time.Now().Before(pending.expiresAt) {
				cp.Status = pending.status
				cp.UpdatedAt = pending.updatedAt
			}
			delete(s.pendingEvents, incoming.PaneID)
		}

		if !exists {
			cp.UpdatedAt = 0
		} else if existing.Status == cp.Status && existing.Name == cp.Name && existing.Cwd == cp.Cwd && existing.Agent == cp.Agent &&
			existing.PaneRevision == cp.PaneRevision && existing.ScrollMaxOffset == cp.ScrollMaxOffset && existing.ForegroundCwd == cp.ForegroundCwd {
			cp.UpdatedAt = existing.UpdatedAt
		} else {
			cp.UpdatedAt = time.Now().UnixMilli()
		}

		s.applyBlockedCycleLocked(&cp, existing)

		if !exists || existing.Status != cp.Status || existing.Name != cp.Name || existing.Cwd != cp.Cwd || existing.Agent != cp.Agent {
			s.contentRev[incoming.PaneID]++
		}

		prev := s.prevStatus[incoming.PaneID]
		s.revision[incoming.PaneID] = s.revCounter
		s.agents[incoming.PaneID] = &cp
		s.prevStatus[incoming.PaneID] = cp.Status
		s.registerTransition(incoming.PaneID, prev, cp.Status)
	}

	for id := range s.agents {
		if !seen[id] {
			delete(s.agents, id)
			delete(s.revision, id)
			delete(s.contentRev, id)
			delete(s.prevStatus, id)
			delete(s.unseenDone, id)
			delete(s.ackDone, id)
			delete(s.finishedNotif, id)
		}
	}
	now := time.Now()
	for paneID, pending := range s.pendingEvents {
		if !now.Before(pending.expiresAt) {
			delete(s.pendingEvents, paneID)
		}
	}
}

// CommitEvent applies an authoritative status update from a UDP event. These
// take precedence over in-flight inventory polls (§10.3).
func (s *State) CommitEvent(paneID, status string, updatedAt int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.agents[paneID]; !exists {
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
			status:    status,
			updatedAt: updatedAt,
			expiresAt: time.Now().Add(30 * time.Second),
		}
		return false
	}
	s.revCounter++
	s.revision[paneID] = s.revCounter

	a := s.agents[paneID]
	prev := a.Status
	a.Status = status
	a.UpdatedAt = updatedAt
	if prev != status {
		s.contentRev[paneID]++
	}
	if status == "blocked" {
		if prev != "blocked" || a.BlockedEventID == "" {
			a.BlockedEventID = s.newBlockedEventIDLocked()
		}
	} else {
		clearBlockedDetails(a)
	}
	s.prevStatus[paneID] = status
	s.registerTransition(paneID, prev, status)
	return true
}

func (s *State) applyBlockedCycleLocked(agent, existing *AgentState) {
	if agent.Status != "blocked" {
		clearBlockedDetails(agent)
		return
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
	agent.Prompt = ""
	agent.Command = ""
	agent.Options = nil
	agent.Interaction = nil
	agent.QuestionLayout = false
	agent.InteractionID = ""
}

// registerTransition implements the once-per-cycle notification state machine.
// Blocked notifications fire only on actual transitions into "blocked" (§16.13).
// Completion (working/blocked → idle) marks the pane as unseen-done (§9.8).
func (s *State) registerTransition(paneID, prev, status string) {
	if attentionStatuses[status] {
		delete(s.unseenDone, paneID)
		delete(s.ackDone, paneID)
		delete(s.finishedNotif, paneID)
		if status == "blocked" && prev != "blocked" && s.onTransition != nil {
			a := s.agents[paneID]
			agent, project := "", ""
			if a != nil {
				agent, project = a.Agent, a.Project
			}
			go s.onTransition(paneID, agent, project, status, s.revision[paneID])
		}
		return
	}

	if doneStatuses[status] {
		if attentionStatuses[prev] {
			delete(s.ackDone, paneID)
			s.unseenDone[paneID] = true
		}
		if prev != status && s.onTransition != nil {
			a := s.agents[paneID]
			agent, project := "", ""
			if a != nil {
				agent, project = a.Agent, a.Project
			}
			go s.onTransition(paneID, agent, project, status, s.revision[paneID])
		}
		return
	}

	// §9.8: working/blocked → idle is the common completion path for agents
	// that don't emit an explicit "done" status.
	if status == "idle" && attentionStatuses[prev] {
		delete(s.ackDone, paneID)
		s.unseenDone[paneID] = true
		if s.onTransition != nil {
			a := s.agents[paneID]
			agent, project := "", ""
			if a != nil {
				agent, project = a.Agent, a.Project
			}
			go s.onTransition(paneID, agent, project, status, s.revision[paneID])
		}
	}
}

func (s *State) AcknowledgePane(paneID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.agents[paneID]; !exists {
		return false
	}
	if s.unseenDone[paneID] {
		delete(s.unseenDone, paneID)
		s.ackDone[paneID] = true
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
		if cp.Status == "idle" && s.unseenDone[cp.PaneID] {
			cp.Status = "done"
		}
		result = append(result, &cp)
	}
	return result
}

func (s *State) Agent(paneID string) (*AgentState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	agent, ok := s.agents[paneID]
	if !ok {
		return nil, false
	}
	copy := *agent
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
	status string,
	revision int64,
) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	agent := s.agents[paneID]
	if agent == nil || agent.Status != status || s.revision[paneID] != revision || s.finishedNotif[paneID] {
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
