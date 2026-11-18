package coordinator

import (
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/0cv/herdr-mobile-relay/internal/activity"
)

type activityRecorder struct {
	journal   *activity.Journal
	state     *State
	logger    *slog.Logger
	broadcast func(any)
	order     sync.Mutex
	failures  atomic.Uint64
}

func (r *activityRecorder) record(kind, status, summary, paneID, requestID string) {
	r.recordWithExtract(kind, status, summary, "", paneID, requestID)
}

func (r *activityRecorder) recordWithExtract(kind, status, summary, extract, paneID, requestID string) {
	if r.journal == nil {
		return
	}
	r.order.Lock()
	defer r.order.Unlock()
	agentState, _ := r.state.Agent(paneID)
	agent, project, host, session := "", "", "", ""
	if agentState != nil {
		agent = agentState.Agent
		project = agentState.Project
		host = agentState.Host
		session = agentState.Session
	}
	entry := activity.NewEntry(kind, status, summary, paneID, agent, project, requestID)
	entry.Host = host
	entry.Session = session
	entry.Extract = extract
	entry.Details = map[string]any{"action": kind}
	entry = activity.NormalizeEntry(entry)
	if err := r.journal.Append(entry); err != nil {
		r.failures.Add(1)
		r.logger.Warn("activity append failed", "error", err)
		return
	}
	if r.broadcast != nil {
		r.broadcast(map[string]any{"type": "activity", "activity": entry})
	}
}

type TransitionActivity struct {
	Kind            string
	Status          string
	Summary         string
	PaneID          string
	ExpectedStatus  string
	Revision        int64
	Details         map[string]any
	Agent           string
	Project         string
	Host            string
	Session         string
	Extract         string
	BlockedIdentity *BlockedTransitionIdentity
}

type BlockedTransitionIdentity struct {
	EventID           string
	Generation        int64
	AttentionKind     string
	AttentionRevision int64
}

// recordTransition publishes only while the originating state
// revision is still current. If the state advances during the durable append,
// the stale entry is removed before it can be exposed.
func (r *activityRecorder) recordTransition(transition TransitionActivity) bool {
	if r.journal == nil {
		return false
	}
	r.order.Lock()
	defer r.order.Unlock()
	transitionCurrent := func() bool {
		if transition.Kind == "working" {
			return true
		}
		if transition.Kind == "finished" {
			return r.state.CompletionCurrent(transition.PaneID, transition.Revision)
		}
		if identity := transition.BlockedIdentity; transition.ExpectedStatus == "blocked" && identity != nil {
			return r.state.AttentionTransitionCurrent(
				transition.PaneID,
				identity.EventID,
				identity.Generation,
				identity.AttentionKind,
				identity.AttentionRevision,
			)
		}
		return r.state.TransitionCurrent(transition.PaneID, transition.ExpectedStatus, transition.Revision)
	}
	if !transitionCurrent() {
		return false
	}
	entry := activity.NewEntry(transition.Kind, transition.Status, transition.Summary, transition.PaneID, transition.Agent, transition.Project, "")
	entry.Host = transition.Host
	entry.Session = transition.Session
	entry.Details = transition.Details
	if transitionAt, ok := transition.Details["transition_at"].(int64); ok && transitionAt > 0 {
		entry.Timestamp = activity.MilliTimestamp(transitionAt)
	}
	entry.Extract = transition.Extract
	entry = activity.NormalizeEntry(entry)
	if err := r.journal.Append(entry); err != nil {
		r.failures.Add(1)
		r.logger.Warn("transition activity append failed", "error", err)
		return false
	}
	if !transitionCurrent() {
		if discardErr := r.journal.Discard(entry.ID); discardErr != nil {
			r.failures.Add(1)
			r.logger.Warn("stale transition activity discard failed", "error", discardErr)
		}
		return false
	}
	if r.broadcast != nil {
		r.broadcast(map[string]any{"type": "activity", "activity": entry})
	}
	return true
}

func (r *activityRecorder) clear(finish func(error) *CommandResult) *CommandResult {
	r.order.Lock()
	if err := r.journal.Clear(); err != nil {
		r.order.Unlock()
		r.failures.Add(1)
		return finish(err)
	}
	defer r.order.Unlock()
	if r.broadcast != nil {
		r.broadcast(map[string]any{"type": "activity_history", "activities": []any{}})
	}
	return finish(nil)
}
