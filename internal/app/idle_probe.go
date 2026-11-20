package app

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
)

const probeReadsPerCycle = 4

type idleProbeWorker struct {
	server  *Server
	mu      sync.Mutex
	queue   []protocol.TargetRef
	served  map[string]bool
	credits int
	wake    chan struct{}
	now     func() time.Time
}

func (s *Server) configureIdleProbe() *idleProbeWorker {
	s.logger.Info("idle question probe", "enabled", s.cfg.IdleQuestionProbe)
	if !s.cfg.IdleQuestionProbe {
		return nil
	}
	s.state.EnableIdleQuestionProbe()
	worker := &idleProbeWorker{server: s, wake: make(chan struct{}, 1), now: time.Now}
	s.poller.SetPostCommit(worker.reconcile, worker.refresh)
	return worker
}

func (w *idleProbeWorker) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *idleProbeWorker) reconcile(committed bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !committed {
		w.credits = 0
		w.server.state.ResolvePendingProbes(w.now())
		return
	}
	w.credits = probeReadsPerCycle
	w.served = make(map[string]bool, probeReadsPerCycle)
	w.refreshLocked()
	w.signal()
}

func (w *idleProbeWorker) refresh() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.refreshLocked()
	w.signal()
}

func (w *idleProbeWorker) refreshLocked() {
	var eligible []protocol.TargetRef
	current := make(map[protocol.TargetRef]bool)
	for _, a := range w.server.state.Snapshot() {
		if w.eligible(a) {
			target := a.TargetRef()
			current[target] = true
			eligible = append(eligible, target)
		}
	}
	w.queue = slices.DeleteFunc(w.queue, func(target protocol.TargetRef) bool {
		return !current[target]
	})
	for _, target := range w.queue {
		delete(current, target)
	}
	for _, target := range eligible {
		if current[target] {
			w.queue = append(w.queue, target)
		}
	}
}

func (w *idleProbeWorker) offer(paneID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	agent, ok := w.server.state.Agent(paneID)
	eligible := ok && w.eligible(agent)
	w.queue = slices.DeleteFunc(w.queue, func(target protocol.TargetRef) bool {
		return target.PaneID == paneID && (!eligible || target != agent.TargetRef())
	})
	if eligible && !slices.Contains(w.queue, agent.TargetRef()) {
		w.queue = append(w.queue, agent.TargetRef())
	}
	w.signal()
}

func (w *idleProbeWorker) leased(target protocol.TargetRef) bool {
	return w.server.paneSizeM != nil && w.server.paneSizeM.Leased(target.PaneID, target.TerminalID)
}

func (w *idleProbeWorker) eligible(agent *coordinator.AgentState) bool {
	return agent.ProbeGoverned() && (agent.ProbePending || agent.ScreenAttention == coordinator.ScreenWaiting || w.leased(agent.TargetRef()))
}

func (w *idleProbeWorker) next() (protocol.TargetRef, coordinator.ProbeToken, string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i := 0; w.credits > 0 && i < len(w.queue); {
		target := w.queue[i]
		if w.served[target.PaneID] {
			i++
			continue
		}
		w.queue = slices.Delete(w.queue, i, i+1)
		token, kind, ok := w.server.state.ProbeToken(target, w.leased(target))
		if !ok {
			continue
		}
		w.credits--
		w.served[target.PaneID] = true
		w.queue = append(w.queue, target)
		return target, token, kind, true
	}
	return protocol.TargetRef{}, coordinator.ProbeToken{}, "", false
}

func (w *idleProbeWorker) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.wake:
			for ctx.Err() == nil && w.readNext(ctx) {
			}
		}
	}
}

func (w *idleProbeWorker) readNext(ctx context.Context) bool {
	target, token, agent, ok := w.next()
	if !ok || ctx.Err() != nil {
		return false
	}
	classification, err := coordinator.ReadLiveAttention(ctx, w.server.herdrC, target.PaneID, agent)
	if ctx.Err() != nil {
		return false
	}
	if w.server.state.CommitProbe(token, classification, err == nil, w.leased(target), w.now()) {
		if err := w.server.publishCurrentInventory(ctx); err != nil {
			w.server.logger.Warn("probe inventory publication failed", "error", err)
		}
	}
	return true
}
