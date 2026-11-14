package coordinator

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/0cv/herdr-mobile-relay/internal/activity"
)

type activityRecorder struct {
	worker    *activity.Worker
	state     *State
	logger    *slog.Logger
	broadcast func(any)
	order     sync.Mutex
	sequence  atomic.Uint64
	failures  atomic.Uint64
}

func (r *activityRecorder) record(kind, status, summary, paneID, requestID string) {
	r.recordWithExtract(kind, status, summary, "", paneID, requestID)
}

func (r *activityRecorder) recordWithExtract(kind, status, summary, extract, paneID, requestID string) {
	if r.worker == nil {
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
	committed, err := r.worker.Commit(context.Background(), activity.ActivityCommitRequested{
		Sequence: r.sequence.Add(1),
		Entry:    entry,
	})
	if err != nil {
		r.failures.Add(1)
		r.logger.Warn("activity append failed", "error", err)
		return
	}
	if r.broadcast != nil {
		r.broadcast(map[string]any{"type": "activity", "activity": committed.Entry})
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
// the worker tombstones and removes the stale record before it can be exposed.
func (r *activityRecorder) recordTransition(transition TransitionActivity) bool {
	if r.worker == nil {
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
	committed, err := r.worker.Commit(context.Background(), activity.ActivityCommitRequested{
		Sequence: r.sequence.Add(1),
		Entry:    entry,
	})
	if err != nil {
		r.failures.Add(1)
		r.logger.Warn("transition activity append failed", "error", err)
		return false
	}
	if !transitionCurrent() {
		_, discardErr := r.worker.Discard(context.Background(), activity.ActivityDiscardRequested{
			Sequence: r.sequence.Add(1),
			ID:       committed.Entry.ID,
		})
		if discardErr != nil {
			r.failures.Add(1)
			r.logger.Warn("stale transition activity discard failed", "error", discardErr)
		}
		return false
	}
	if r.broadcast != nil {
		r.broadcast(map[string]any{"type": "activity", "activity": committed.Entry})
	}
	return true
}

func (r *activityRecorder) clear(finish func(error) *CommandResult) *CommandResult {
	r.order.Lock()
	defer r.order.Unlock()
	if _, err := r.worker.Clear(context.Background(), activity.ActivityClearRequested{
		Sequence: r.sequence.Add(1),
	}); err != nil {
		r.failures.Add(1)
		return finish(err)
	}
	if r.broadcast != nil {
		r.broadcast(map[string]any{"type": "activity_history", "activities": []any{}})
	}
	return finish(nil)
}

func (r *activityRecorder) close(ctx context.Context) error {
	if r.worker == nil {
		return nil
	}
	return r.worker.Close(ctx)
}
