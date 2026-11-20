package coordinator

import (
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/question"
)

const (
	ScreenWaiting = "waiting"
	ScreenCleared = "cleared"
	ProbeTimeout  = 3 * time.Second
)

type idleProbeState struct {
	stop      int64
	serial    uint64
	firstMiss time.Time
}

type ProbeTarget struct {
	PaneID     string
	Generation int64
	TerminalID string
	SessionID  string
}

func (a *AgentState) ProbeTarget() ProbeTarget {
	return ProbeTarget{a.PaneID, a.Generation, a.TerminalID, a.SessionID}
}

func idleStatus(status string) bool {
	return status == "idle" || doneStatuses[status]
}

func (a *AgentState) IdleProbeCandidate() bool {
	return idleStatus(a.RawStatus) && question.Supports(a.Agent)
}

func (a *AgentState) ProbeGoverned() bool {
	return a.ProbeEnabled && a.IdleProbeCandidate()
}

func LiveIdentity(c question.Classification) string {
	if c.Interaction != nil && c.Kind == question.AttentionQuestion {
		return c.Interaction.ID
	}
	return question.ApprovalFingerprint(c)
}

func (s *State) EnableIdleQuestionProbe() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.idleProbeEnabled = true
}

func (s *State) applyRawStatusLocked(rec *paneRecord, next, previous *AgentState, nativeSample bool) {
	raw, oldRaw := next.Status, rec.rawStatus
	rec.rawStatus = raw
	next.RawStatus = raw
	next.ProbeEnabled = s.idleProbeEnabled && question.Supports(next.Agent)
	if !next.ProbeEnabled || previous == nil {
		return
	}
	next.ScreenAttention = previous.ScreenAttention
	if idleStatus(raw) {
		if rec.probe.stop == 0 && previous.ScreenAttention != ScreenWaiting && attentionStatuses[oldRaw] {
			rec.probe.stop = s.revCounter
		}
		if rec.probe.stop != 0 || previous.ScreenAttention == ScreenWaiting {
			next.Status = previous.Status
			copyBlockedDetails(next, previous)
			next.ProbePending = rec.probe.stop != 0
			return
		}
	}
	if raw == "blocked" {
		next.ScreenAttention = ""
		if !nativeSample && previous.Status == "blocked" {
			copyBlockedDetails(next, previous)
		} else if previous.StrictAttention && next.AttentionKind == previous.AttentionKind {
			if next.LiveIdentity != "" && next.LiveIdentity == previous.LiveIdentity {
				next.StrictAttention = true
				next.ApprovalFingerprint = previous.ApprovalFingerprint
			} else if next.AttentionKind == question.AttentionApproval {
				next.BlockedEventID = s.newBlockedEventIDLocked()
			}
		}
	} else if previous.ScreenAttention == ScreenWaiting || rec.probe.stop != 0 && previous.Status == "blocked" {
		next.ScreenAttention = ScreenCleared
	}
	next.ProbePending = false
	if rec.probe.stop != 0 || previous.ScreenAttention == ScreenWaiting {
		rec.probe.serial++
	}
	rec.probe.stop = 0
	rec.probe.firstMiss = time.Time{}
}

type ProbeToken struct {
	target  ProbeTarget
	stop    int64
	serial  uint64
	content int64
	event   string
}

func (s *State) ProbeToken(target ProbeTarget, leased bool) (ProbeToken, string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec := s.probePaneLocked(target)
	if rec == nil || !rec.agent.ProbeGoverned() || !probeEligible(rec, leased) {
		return ProbeToken{}, "", false
	}
	return ProbeToken{target, rec.probe.stop, rec.probe.serial, rec.contentRev, rec.agent.BlockedEventID}, rec.agent.Agent, true
}

func probeEligible(rec *paneRecord, leased bool) bool {
	return rec.probe.stop != 0 || rec.agent.ScreenAttention == ScreenWaiting || leased
}

func (s *State) probePaneLocked(target ProbeTarget) *paneRecord {
	rec := s.panes[target.PaneID]
	if rec == nil || s.generation[target.PaneID] != target.Generation ||
		rec.agent.TerminalID != target.TerminalID || rec.agent.SessionID != target.SessionID {
		return nil
	}
	return rec
}

func (s *State) CommitProbe(token ProbeToken, classification question.Classification, reliable, leased bool, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.probePaneLocked(token.target)
	if rec == nil || !rec.agent.ProbeGoverned() {
		return false
	}
	current := token.stop == rec.probe.stop && token.serial == rec.probe.serial &&
		token.content == rec.contentRev && token.event == rec.agent.BlockedEventID && probeEligible(rec, leased)
	identity := LiveIdentity(classification)
	if current && reliable && identity != "" {
		return s.promoteProbeLocked(rec, classification, identity, now)
	}
	if token.stop != 0 && token.stop == rec.probe.stop {
		s.endProbeLocked(rec, now)
		return true
	}
	if !current || !reliable || rec.agent.ScreenAttention != ScreenWaiting {
		return false
	}
	rec.probe.serial++
	if rec.probe.firstMiss.IsZero() {
		rec.probe.firstMiss = now
	} else if now.Sub(rec.probe.firstMiss) >= 2*time.Second {
		s.endProbeLocked(rec, now)
		return true
	}
	return false
}

func (s *State) promoteProbeLocked(rec *paneRecord, classification question.Classification, identity string, now time.Time) bool {
	prev := rec.agent
	next := *prev
	next.Status = "blocked"
	next.ScreenAttention = ScreenWaiting
	next.ProbePending = false
	next.LiveIdentity = identity
	next.StrictAttention = true
	previousIdentity := prev.LiveIdentity
	if previousIdentity == "" && prev.AttentionKind == question.AttentionQuestion {
		previousIdentity = prev.InteractionID
	}
	same := prev.AttentionKind == classification.Kind && previousIdentity == identity
	newEvent := prev.Status != "blocked" || prev.AttentionKind != classification.Kind ||
		classification.Kind == question.AttentionApproval && !same
	if newEvent {
		next.BlockedEventID = s.newBlockedEventIDLocked()
	} else if same && classification.Kind == question.AttentionApproval {
		classification.ApprovalIdentity = prev.ApprovalFingerprint
	}
	changed := applyAttentionClassification(&next, classification)
	rec.probe.serial++
	rec.probe.stop = 0
	rec.probe.firstMiss = time.Time{}
	if !changed && !newEvent && prev.ScreenAttention == ScreenWaiting {
		return false
	}
	s.commitProbeStateLocked(rec, &next, newEvent, now)
	return true
}

func (s *State) endProbeLocked(rec *paneRecord, now time.Time) {
	next := *rec.agent
	if next.Status == "blocked" {
		next.ScreenAttention = ScreenCleared
	}
	next.Status = rec.rawStatus
	next.ProbePending = false
	clearBlockedDetails(&next)
	rec.probe.stop = 0
	rec.probe.firstMiss = time.Time{}
	rec.probe.serial++
	s.commitProbeStateLocked(rec, &next, false, now)
}

func (s *State) commitProbeStateLocked(rec *paneRecord, next *AgentState, newEvent bool, now time.Time) {
	previous := rec.agent
	s.revCounter++
	rec.revision = s.revCounter
	rec.contentRev++
	rec.attentionRev++
	next.UpdatedAt = now.UnixMilli()
	next.LastActiveAt = max(next.LastActiveAt, next.UpdatedAt)
	rec.agent = next
	s.registerTransition(next.PaneID, previous.Status, next.Status, previous.AttentionKind)
	if !preservesChatCompletion(previous.Status, next.Status, previous.AttentionKind) {
		s.syncAttentionCompletionLocked(next.PaneID, previous.AttentionKind, next.AttentionKind)
	}
	if newEvent && previous.Status == next.Status {
		s.fireTransition(next.PaneID, next.Status)
	}
	if s.syncTriageLocked(next) {
		s.persistTriageLocked()
	}
}

func (s *State) ResolvePendingProbes(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for _, rec := range s.panes {
		if rec.probe.stop != 0 {
			s.endProbeLocked(rec, now)
			changed = true
		}
	}
	return changed
}
