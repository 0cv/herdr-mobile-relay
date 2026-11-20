package app

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/question"
)

const probeReadsPerCycle = 4

type idleProbeWorker struct {
	server  *Server
	mu      sync.Mutex
	queue   []coordinator.ProbeTarget
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
	var eligible []coordinator.ProbeTarget
	current := make(map[coordinator.ProbeTarget]bool)
	for _, a := range w.server.state.Snapshot() {
		if w.eligible(a) {
			target := a.ProbeTarget()
			current[target] = true
			eligible = append(eligible, target)
		}
	}
	w.queue = slices.DeleteFunc(w.queue, func(target coordinator.ProbeTarget) bool {
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
	w.queue = slices.DeleteFunc(w.queue, func(target coordinator.ProbeTarget) bool {
		return target.PaneID == paneID && (!eligible || target != agent.ProbeTarget())
	})
	if eligible && !slices.Contains(w.queue, agent.ProbeTarget()) {
		w.queue = append(w.queue, agent.ProbeTarget())
	}
	w.signal()
}

func (w *idleProbeWorker) leased(target coordinator.ProbeTarget) bool {
	return w.server.paneSizeM != nil && w.server.paneSizeM.Leased(target.PaneID, target.TerminalID)
}

func (w *idleProbeWorker) eligible(agent *coordinator.AgentState) bool {
	return agent.ProbeGoverned() && (agent.ProbePending || agent.ScreenAttention == coordinator.ScreenWaiting || w.leased(agent.ProbeTarget()))
}

func (w *idleProbeWorker) next() (coordinator.ProbeTarget, coordinator.ProbeToken, string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i := 0; w.credits > 0 && i < len(w.queue); {
		target := w.queue[i]
		if w.served[target.PaneID] {
			i++
			continue
		}
		agent, exists := w.server.state.Agent(target.PaneID)
		w.queue = slices.Delete(w.queue, i, i+1)
		if !exists || agent.ProbeTarget() != target || !agent.ProbeGoverned() {
			continue
		}
		leased := !agent.ProbePending && agent.ScreenAttention != coordinator.ScreenWaiting && w.leased(target)
		token, kind, ok := w.server.state.ProbeToken(target, leased)
		if !ok {
			continue
		}
		w.credits--
		w.served[target.PaneID] = true
		w.queue = append(w.queue, target)
		return target, token, kind, true
	}
	return coordinator.ProbeTarget{}, coordinator.ProbeToken{}, "", false
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
	readCtx, cancel := context.WithTimeout(ctx, coordinator.ProbeTimeout)
	read, err := w.server.herdrC.ReadPaneVisible(readCtx, target.PaneID, question.VisibleRows, "ansi")
	cancel()
	if ctx.Err() != nil {
		return false
	}
	var classification question.Classification
	var reliable bool
	if err == nil {
		classification, reliable = question.ClassifyLive(string(read.Content), agent)
	}
	if current, exists := w.server.state.Agent(target.PaneID); exists && current.ProbeTarget() == target && current.ProbeGoverned() {
		leased := !current.ProbePending && current.ScreenAttention != coordinator.ScreenWaiting && w.leased(target)
		if w.server.state.CommitProbe(token, classification, reliable, leased, w.now()) {
			if updated, exists := w.server.state.Agent(target.PaneID); exists && updated.ProbeTarget() == target && updated.ScreenAttention != current.ScreenAttention {
				w.server.logger.Info("idle question probe changed attention", "pane_id", target.PaneID, "screen_attention", updated.ScreenAttention, "status", updated.Status)
			}
			if err := w.server.publishCurrentInventory(ctx); err != nil {
				w.server.logger.Warn("probe inventory publication failed", "error", err)
			}
		}
	}
	return true
}
