package coordinator

import (
	"bytes"
	"context"
	"encoding/json"
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
	idlePollInterval          = 15 * time.Second
	maxImmediateTopologyPolls = 3
)

type Poller struct {
	client              *herdr.Client
	state               *State
	logger              *slog.Logger
	interval            time.Duration
	wakeup              chan struct{}
	onChange            func(agents []*AgentState)
	onWorkspaceChange   func(workspaces []herdr.Workspace)
	onStatus            func(status map[string]any)
	enrich              func(context.Context, []*AgentState)
	hostname            string
	topologyRetries     int
	consecutiveFailures atomic.Int32
	eventsActive        atomic.Bool
	// broadcastMu serializes snapshot broadcasts from the reconcile poll and
	// the event stream, and guards the dedupe state below. Snapshots are read
	// inside the lock so a slow commit path can never publish an older
	// topology after a newer one already went out.
	broadcastMu        sync.Mutex
	lastAgentsJSON     []byte
	lastWorkspacesJSON []byte
}

func NewPoller(client *herdr.Client, state *State, interval time.Duration, logger *slog.Logger) *Poller {
	hostname, _ := os.Hostname()
	if idx := strings.Index(hostname, "."); idx > 0 {
		hostname = hostname[:idx]
	}
	return &Poller{
		client:   client,
		state:    state,
		logger:   logger,
		interval: interval,
		wakeup:   make(chan struct{}, 1),
		hostname: hostname,
	}
}

func (p *Poller) SetOnChange(fn func(agents []*AgentState)) {
	p.onChange = fn
}

func (p *Poller) SetOnWorkspaceChange(fn func(workspaces []herdr.Workspace)) {
	p.onWorkspaceChange = fn
}

func (p *Poller) SetOnInventoryStatus(fn func(status map[string]any)) {
	p.onStatus = fn
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
	p.poll(ctx)

	timer := time.NewTimer(p.currentInterval())
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-p.wakeup:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			p.poll(ctx)
			timer.Reset(p.currentInterval())
		case <-timer.C:
			p.poll(ctx)
			timer.Reset(p.currentInterval())
		}
	}
}

func (p *Poller) poll(ctx context.Context) {
	token := p.state.BeginPoll()
	previousStatus := p.state.InventoryStatus()

	inv, err := p.client.GetInventory(ctx)
	if err != nil {
		p.consecutiveFailures.Add(1)
		p.state.MarkInventoryFailure(err)
		p.notifyStatusChange(previousStatus)
		p.logger.Warn("inventory poll failed", "error", err)
		return
	}
	p.consecutiveFailures.Store(0)

	workspaces, err := p.client.WorkspaceList(ctx)
	if err != nil {
		p.consecutiveFailures.Add(1)
		p.state.MarkInventoryFailure(err)
		p.notifyStatusChange(previousStatus)
		p.logger.Warn("workspace inventory poll failed", "error", err)
		return
	}

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

	workspaceChanged, committed := p.state.CommitPoll(agents, workspaces, token)
	if !committed {
		p.logger.Debug("discarded topology-stale inventory sample")
		p.handleTopologyStale(previousStatus)
		return
	}
	p.topologyRetries = 0
	p.notifyStatusChange(previousStatus)
	p.logger.Debug("inventory committed", "agents", len(agents), "topology", p.state.TopologyGeneration())

	p.notifyAgentsChanged()
	if workspaceChanged {
		p.notifyWorkspacesChanged()
	}
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
			if !waitForEventReconnect(ctx) {
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
		if !waitForEventReconnect(ctx) {
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
	previousStatus := p.state.InventoryStatus()
	agents := p.agentsFromTopology(topology.Panes, topology.Tabs)
	if p.enrich != nil {
		p.enrich(ctx, agents)
	}
	p.consecutiveFailures.Store(0)
	workspaceChanged := p.state.CommitTopology(agents, topology.Workspaces, baseRevision)
	p.notifyStatusChange(previousStatus)
	p.logger.Debug("event inventory committed", "agents", len(agents), "workspaces", len(topology.Workspaces), "topology", p.state.TopologyGeneration())
	p.notifyAgentsChanged()
	if workspaceChanged {
		p.notifyWorkspacesChanged()
	}
}

// notifyAgentsChanged broadcasts the current agent snapshot unless nothing a
// client renders has changed since the last broadcast. Both freshness sources
// — the reconcile poll and the Herdr event stream — commit through here, so
// an idle machine stops producing a full `agents` push every interval and
// phones on metered or fragile links receive silence instead of a
// re-shuffled copy of what they already display.
//
// StateRevision is excluded from the comparison: every commit stamps every
// agent with the new revision counter, so including it would re-broadcast
// identical inventories forever. A suppressed revision-only bump is safe —
// clients only reject revisions that move backwards.
func (p *Poller) notifyAgentsChanged() {
	if p.onChange == nil {
		return
	}
	p.broadcastMu.Lock()
	defer p.broadcastMu.Unlock()
	snapshot := p.state.Snapshot()
	comparable := make([]AgentState, len(snapshot))
	for i, agent := range snapshot {
		comparable[i] = *agent
		comparable[i].StateRevision = 0
	}
	encoded, err := json.Marshal(comparable)
	if err == nil {
		if bytes.Equal(encoded, p.lastAgentsJSON) {
			return
		}
		p.lastAgentsJSON = encoded
	}
	p.onChange(snapshot)
}

// notifyWorkspacesChanged mirrors notifyAgentsChanged for workspace
// broadcasts: the snapshot is read under broadcastMu so the poll and event
// commits publish in commit order, and a byte-identical broadcast — both
// sources committing the same topology back to back — is suppressed.
func (p *Poller) notifyWorkspacesChanged() {
	if p.onWorkspaceChange == nil {
		return
	}
	p.broadcastMu.Lock()
	defer p.broadcastMu.Unlock()
	workspaces := p.state.Workspaces()
	encoded, err := json.Marshal(workspaces)
	if err == nil {
		if bytes.Equal(encoded, p.lastWorkspacesJSON) {
			return
		}
		p.lastWorkspacesJSON = encoded
	}
	p.onWorkspaceChange(workspaces)
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

func (p *Poller) handleTopologyStale(previousStatus map[string]any) {
	p.topologyRetries++
	if p.topologyRetries <= maxImmediateTopologyPolls {
		p.Wake()
		return
	}
	p.state.MarkTopologyDegraded()
	p.notifyStatusChange(previousStatus)
	p.logger.Warn("inventory topology did not stabilize", "immediate_retries", maxImmediateTopologyPolls)
}

func (p *Poller) notifyStatusChange(previous map[string]any) {
	current := p.state.InventoryStatus()
	if p.onStatus != nil && inventoryStatusChanged(previous, current) {
		p.onStatus(current)
	}
}

func inventoryStatusChanged(previous, current map[string]any) bool {
	for _, key := range []string{"state", "error_code", "message", "stale"} {
		if previous[key] != current[key] {
			return true
		}
	}
	return false
}

// currentInterval keeps the reconcile poll slow while the event stream is
// healthy. When events are unavailable the poll is the only freshness source
// again, so the operator-configured interval is honoured.
func (p *Poller) currentInterval() time.Duration {
	if p.eventsActive.Load() {
		return idlePollInterval
	}
	if p.interval <= 0 || p.interval > idlePollInterval {
		return idlePollInterval
	}
	return p.interval
}
