package coordinator

import (
	"context"
	"errors"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/backend"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/question"
)

const (
	ScreenWaiting = "waiting"
	ScreenCleared = "cleared"
)

type idleProbeState struct {
	stop      int64
	firstMiss time.Time
}

func ReadLiveAttention(ctx context.Context, client backend.Backend, paneID, agent string) (question.Classification, error) {
	readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	read, err := client.ReadPaneVisible(readCtx, paneID, question.VisibleRows, "ansi")
	if err != nil {
		return question.Classification{}, err
	}
	classification, reliable := question.ClassifyLive(string(read.Content), agent)
	if !reliable {
		return question.Classification{}, errors.New("The visible screen could not be verified")
	}
	return classification, nil
}

func idleStatus(status string) bool {
	return status == "idle" || doneStatuses[status]
}

func (a *AgentState) ProbeGoverned() bool {
	return a.ProbeEnabled && idleStatus(a.RawStatus)
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

func (s *State) idleProbeEnabledForQuestions() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.idleProbeEnabled
}

func (s *State) applyRawStatusLocked(rec *paneRecord, next, previous *AgentState, nativeSample bool) {
	raw := next.Status
	next.RawStatus = raw
	next.ProbeEnabled = s.idleProbeEnabled && question.Supports(next.Agent)
	if !next.ProbeEnabled || previous == nil {
		rec.probe = idleProbeState{}
		return
	}
	next.ScreenAttention = previous.ScreenAttention
	if idleStatus(raw) {
		if rec.probe.stop == 0 && previous.ScreenAttention != ScreenWaiting && attentionStatuses[previous.RawStatus] {
			rec.probe.stop = s.revCounter
		}
		if rec.probe.stop != 0 || previous.ScreenAttention == ScreenWaiting {
			next.Status = previous.Status
			copyBlockedDetails(next, previous)
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
	} else if previous.StrictAttention || rec.probe.stop != 0 && previous.Status == "blocked" {
		next.ScreenAttention = ScreenCleared
	}
	rec.probe.stop = 0
	rec.probe.firstMiss = time.Time{}
}

type ProbeToken struct {
	target  protocol.TargetRef
	stop    int64
	content int64
	event   string
}

func (s *State) ProbeToken(target protocol.TargetRef, leased bool) (ProbeToken, string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec := s.probePaneLocked(target)
	if rec == nil || !rec.agent.ProbeGoverned() || !probeEligible(rec, leased) {
		return ProbeToken{}, "", false
	}
	return ProbeToken{target, rec.probe.stop, rec.contentRev, rec.agent.BlockedEventID}, rec.agent.Agent, true
}

func probeEligible(rec *paneRecord, leased bool) bool {
	return rec.probe.stop != 0 || rec.agent.ScreenAttention == ScreenWaiting || leased
}

func (s *State) probePaneLocked(target protocol.TargetRef) *paneRecord {
	rec := s.panes[target.PaneID]
	if rec == nil || !rec.agent.matchesTarget(target, s.generation[target.PaneID]) {
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
	previous := rec.agent.ScreenAttention
	defer func() {
		if marker := rec.agent.ScreenAttention; marker != previous && s.logger != nil {
			s.logger.Info("idle question probe changed attention", "pane_id", token.target.PaneID, "screen_attention", marker, "status", rec.agent.Status)
		}
	}()
	current := token.stop == rec.probe.stop && token.content == rec.contentRev && token.event == rec.agent.BlockedEventID && probeEligible(rec, leased)
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
	next.Status = next.RawStatus
	clearBlockedDetails(&next)
	rec.probe.stop = 0
	rec.probe.firstMiss = time.Time{}
	s.commitProbeStateLocked(rec, &next, false, now)
}

func (s *State) commitProbeStateLocked(rec *paneRecord, next *AgentState, newEvent bool, now time.Time) {
	previous := rec.agent
	s.revCounter++
	rec.revision = s.revCounter
	rec.contentRev++
	rec.attentionRev++
	next.UpdatedAt = now.UnixMilli()
	rec.agent = next
	s.registerTransition(next.PaneID, previous.Status, next.Status, previous.AttentionKind)
	if !preservesChatCompletion(previous.Status, next.Status, previous.AttentionKind) {
		s.syncAttentionCompletionLocked(next.PaneID, previous.AttentionKind, next.AttentionKind)
	}
	if newEvent && previous.Status == next.Status {
		s.fireTransition(next.PaneID, next.Status)
	}
}

func (s *State) ResolvePendingProbes(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rec := range s.panes {
		if rec.probe.stop != 0 {
			s.endProbeLocked(rec, now)
		}
	}
}
