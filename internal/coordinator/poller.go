package coordinator

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

const (
	idlePollInterval     = 15 * time.Second
	maxPollRetryInterval = 60 * time.Second
	// Even a 1ns healthy interval reaches the retry cap before 64 doublings.
	maxPollRetryFailures      = 64
	maxImmediateTopologyPolls = 3
)

type Poller struct {
	client             *herdr.Client
	state              *State
	logger             *slog.Logger
	interval           time.Duration
	wakeup             chan struct{}
	eventReconnectWait func(context.Context) bool
	onInventoryChange  func() error
	enrich             func(context.Context, []*AgentState)
	hostname           string
	topologyRetries    int
	// Owned by the polling goroutine; event commits must not reset it.
	pollRetryFailures   int
	consecutiveFailures atomic.Int32
	eventsActive        atomic.Bool
	broadcastMu         sync.Mutex
}

func NewPoller(client *herdr.Client, state *State, interval time.Duration, logger *slog.Logger) *Poller {
	hostname, _ := os.Hostname()
	if idx := strings.Index(hostname, "."); idx > 0 {
		hostname = hostname[:idx]
	}
	return &Poller{
		client:             client,
		state:              state,
		interval:           interval,
		wakeup:             make(chan struct{}, 1),
		eventReconnectWait: waitForEventReconnect,
		logger:             logger,
		hostname:           hostname,
	}
}

// SetOnInventoryChange installs the single current-state publication signal.
// The callback deliberately carries no captured payload: it must select state
// after the operation that caused the signal has completed.
func (p *Poller) SetOnInventoryChange(fn func() error) {
	p.onInventoryChange = fn
}

func (p *Poller) SetEnrich(fn func(context.Context, []*AgentState)) {
	p.enrich = fn
}

func (p *Poller) Wake() {
	select {
	case p.wakeup <- struct{}{}:
	default:
	}
}

func (p *Poller) ConsecutiveFailures() int {
	return int(p.consecutiveFailures.Load())
}

func (p *Poller) Run(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	p.poll(ctx)
	if ctx.Err() != nil {
		return
	}

	timer := time.NewTimer(p.nextPollInterval())
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-p.wakeup:
			stopPollTimer(timer)
			if ctx.Err() != nil {
				return
			}
			p.poll(ctx)
			if ctx.Err() != nil {
				return
			}
			resetPollTimer(timer, p.nextPollInterval())
		case <-timer.C:
			if ctx.Err() != nil {
				return
			}
			p.poll(ctx)
			if ctx.Err() != nil {
				return
			}
			resetPollTimer(timer, p.nextPollInterval())
		}
	}
}

func (p *Poller) poll(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	token := p.state.BeginPoll()

	inv, err := p.client.GetInventory(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		p.recordPollFailure()
		p.state.MarkInventoryFailure(err)
		p.notifyInventoryChange()
		p.logger.Warn("inventory poll failed", "error", err)
		return
	}

	workspaces, err := p.client.WorkspaceList(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		p.recordPollFailure()
		p.state.MarkInventoryFailure(err)
		p.notifyInventoryChange()
		p.logger.Warn("workspace inventory poll failed", "error", err)
		return
	}
	p.resetPollFailures()

	tabs, tabErr := p.client.TabList(ctx)
	if tabErr != nil {
		tabs = nil
	}
	topologyPanes, paneErr := p.client.PaneList(ctx)
	if paneErr != nil {
		topologyPanes = inv.Panes
	}
	hydrateWorkspaceCwds(workspaces, tabs, topologyPanes)
	agents := p.agentsFromTopology(inv.Panes, tabs)

	if p.enrich != nil {
		p.enrich(ctx, agents)
	}

	_, committed := p.state.CommitPoll(agents, workspaces, token)
	if !committed {
		p.logger.Debug("discarded topology-stale inventory sample")
		p.handleTopologyStale()
		p.notifyInventoryChange()
		return
	}
	p.topologyRetries = 0
	p.notifyInventoryChange()
	p.logger.Debug("inventory committed", "agents", len(agents), "topology", p.state.TopologyGeneration())
}

func (p *Poller) agentsFromTopology(panes []herdr.Pane, tabs []herdr.Tab) []*AgentState {
	tabByID := make(map[string]herdr.Tab, len(tabs))
	// 1-based visual position per workspace; tab numbers are stable
	// identities and never reflect moves.
	tabOrderByID := make(map[string]int, len(tabs))
	perWorkspace := make(map[string]int)
	for index, tab := range tabs {
		if tab.Number == 0 {
			tab.Number = index + 1
		}
		tabByID[tab.ID] = tab
		perWorkspace[tab.WorkspaceID]++
		tabOrderByID[tab.ID] = perWorkspace[tab.WorkspaceID]
	}

	agents := make([]*AgentState, 0, len(panes))
	for _, pane := range panes {
		if pane.Agent == "" {
			continue
		}
		if tab, ok := tabByID[pane.TabID]; ok {
			pane.TabLabel = tab.Label
			pane.TabNumber = tab.Number
		}
		project := ""
		if pane.Cwd != "" {
			project = filepath.Base(pane.Cwd)
		}
		agents = append(agents, &AgentState{
			PaneID:          pane.ID,
			RawPaneID:       pane.ID,
			TerminalID:      pane.TerminalID,
			TabID:           pane.TabID,
			TabLabel:        pane.TabLabel,
			TabNumber:       pane.TabNumber,
			TabOrder:        tabOrderByID[pane.TabID],
			WorkspaceID:     pane.WorkspaceID,
			Agent:           pane.Agent,
			Name:            pane.Name,
			Status:          pane.Status,
			Focused:         pane.Focused,
			Cwd:             pane.Cwd,
			Project:         project,
			Host:            p.hostname,
			Session:         pane.Session,
			ActivitySeq:     pane.StateChangeSeq,
			PaneRevision:    pane.Revision,
			ScrollMaxOffset: pane.Scroll.MaxOffsetFromBottom,
			ForegroundCwd:   pane.ForegroundCwd,
		})
	}
	return agents
}

func hydrateWorkspaceCwds(workspaces []herdr.Workspace, tabs []herdr.Tab, panes []herdr.Pane) {
	cwds := make(map[string]string, len(workspaces))
	for _, tab := range tabs {
		if tab.WorkspaceID != "" {
			cwds[tab.WorkspaceID] = shorterPath(cwds[tab.WorkspaceID], tab.Cwd)
		}
	}
	for _, pane := range panes {
		if pane.WorkspaceID != "" {
			cwds[pane.WorkspaceID] = shorterPath(cwds[pane.WorkspaceID], pane.Cwd)
		}
	}
	for index := range workspaces {
		if workspaces[index].Worktree != nil && workspaces[index].Worktree.CheckoutPath != "" {
			workspaces[index].Cwd = workspaces[index].Worktree.CheckoutPath
			continue
		}
		workspaces[index].Cwd = cwds[workspaces[index].ID]
	}
}

func shorterPath(current, candidate string) string {
	if candidate == "" || current != "" && len(current) <= len(candidate) {
		return current
	}
	return candidate
}

func (p *Poller) RunEvents(ctx context.Context, events *herdr.EventClient) {
	if events == nil {
		return
	}
	defer p.eventsActive.Store(false)
	for {
		if ctx.Err() != nil {
			return
		}
		baseRevision := p.state.RevisionCounter()
		stream, snapshot, buffered, err := events.Bootstrap(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// The reconcile poll is the only freshness source until the stream
			// is back, so let it run at the configured interval again.
			p.eventsActive.Store(false)
			p.logger.Warn("Herdr events stream unavailable", "error", err)
			if !p.eventReconnectWait(ctx) {
				return
			}
			continue
		}
		p.eventsActive.Store(true)

		cache := herdr.NewSessionCache(snapshot)
		p.commitEventTopology(ctx, cache.Snapshot(), baseRevision)
		reconnect := false
		for _, event := range buffered {
			if !p.applyTopologyEvent(ctx, cache, event) {
				reconnect = true
				break
			}
		}
		for !reconnect {
			event, err := stream.Next(ctx)
			if err != nil {
				if ctx.Err() != nil {
					_ = stream.Close()
					return
				}
				p.logger.Warn("Herdr events stream dropped", "error", err)
				reconnect = true
				break
			}
			if !p.applyTopologyEvent(ctx, cache, event) {
				reconnect = true
			}
		}
		p.eventsActive.Store(false)
		_ = stream.Close()
		if !p.eventReconnectWait(ctx) {
			return
		}
	}
}

func (p *Poller) applyTopologyEvent(ctx context.Context, cache *herdr.SessionCache, event herdr.Event) bool {
	changed, err := cache.Apply(event)
	if err != nil {
		p.logger.Warn("Herdr topology event decode failed", "event", event.Event, "error", err)
		return false
	}
	if !changed {
		return true
	}
	p.commitEventTopology(ctx, cache.Snapshot(), p.state.RevisionCounter())
	return true
}

func (p *Poller) commitEventTopology(ctx context.Context, topology herdr.TopologySnapshot, baseRevision int64) {
	agents := p.agentsFromTopology(topology.Panes, topology.Tabs)
	if p.enrich != nil {
		p.enrich(ctx, agents)
	}
	// Event topology is an independent health source. It may recover the
	// public poll-health counter, but must not erase a reconciliation retry
	// streak while required polling fetches are failing.
	p.consecutiveFailures.Store(0)
	p.state.CommitTopology(agents, topology.Workspaces, baseRevision)
	p.notifyInventoryChange()
	p.logger.Debug("event inventory committed", "agents", len(agents), "workspaces", len(topology.Workspaces), "topology", p.state.TopologyGeneration())
}

func waitForEventReconnect(ctx context.Context) bool {
	timer := time.NewTimer(idlePollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (p *Poller) handleTopologyStale(_ ...map[string]any) {
	p.topologyRetries++
	if p.topologyRetries <= maxImmediateTopologyPolls {
		p.Wake()
		return
	}
	p.state.MarkTopologyDegraded()
	p.logger.Warn("inventory topology did not stabilize", "immediate_retries", maxImmediateTopologyPolls)
}

// notifyInventoryChange serializes every publication signal. State mutations
// happen before this method is called and the callback must capture current
// state itself, so no State mutex is held while user code or transport runs.
func (p *Poller) notifyInventoryChange() {
	p.broadcastMu.Lock()
	defer p.broadcastMu.Unlock()
	if p.onInventoryChange == nil {
		return
	}
	if err := p.onInventoryChange(); err != nil {
		p.logger.Warn("inventory publication failed", "error", err)
	}
}

func (p *Poller) recordPollFailure() {
	if p.pollRetryFailures < maxPollRetryFailures {
		p.pollRetryFailures++
	}
	p.consecutiveFailures.Add(1)
}

func (p *Poller) resetPollFailures() {
	p.pollRetryFailures = 0
	p.consecutiveFailures.Store(0)
}

func stopPollTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func resetPollTimer(timer *time.Timer, delay time.Duration) {
	stopPollTimer(timer)
	timer.Reset(delay)
}

// currentInterval keeps the reconcile poll slow while the event stream is
// healthy. When events are unavailable the poll is the only freshness source
// again, so the operator-configured interval is honoured.
func (p *Poller) currentInterval() time.Duration {
	if p.eventsActive.Load() {
		return idlePollInterval
	}
	return normalizePollInterval(p.interval)
}

func (p *Poller) nextPollInterval() time.Duration {
	return pollRetryInterval(p.currentInterval(), p.pollRetryFailures)
}

func normalizePollInterval(interval time.Duration) time.Duration {
	if interval <= 0 || interval > idlePollInterval {
		return idlePollInterval
	}
	return interval
}

// pollRetryInterval doubles the healthy interval once for each failed required
// poll, stopping at the outage cap. The guard before multiplication keeps the
// calculation safe even for a very large failure streak.
func pollRetryInterval(healthyInterval time.Duration, failures int) time.Duration {
	interval := normalizePollInterval(healthyInterval)
	for ; failures > 0; failures-- {
		if interval >= maxPollRetryInterval || interval > maxPollRetryInterval/2 {
			return maxPollRetryInterval
		}
		interval *= 2
	}
	return interval
}
