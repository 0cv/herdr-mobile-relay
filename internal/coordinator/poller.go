package coordinator

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

const (
	idlePollInterval          = 15 * time.Second
	maxImmediateTopologyPolls = 3
	pollFailureBackoffAfter   = 3
)

type Poller struct {
	client              *herdr.Client
	state               *State
	logger              *slog.Logger
	interval            time.Duration
	wakeup              chan struct{}
	onChange            func(agents []*AgentState)
	onStatus            func(status map[string]any)
	enrich              func(context.Context, []*AgentState)
	hostname            string
	topologyRetries     int
	pollFailures        int
	consecutiveFailures atomic.Int32
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
		p.pollFailures++
		p.consecutiveFailures.Store(int32(p.pollFailures))
		p.state.MarkInventoryFailure(err)
		p.notifyStatusChange(previousStatus)
		p.logger.Warn("inventory poll failed", "error", err)
		return
	}
	p.pollFailures = 0
	p.consecutiveFailures.Store(0)

	agents := make([]*AgentState, 0, len(inv.Panes))

	tabs, tabErr := p.client.TabList(ctx)
	var tabByID map[string]herdr.Tab
	if tabErr == nil {
		tabByID = make(map[string]herdr.Tab, len(tabs))
		for i, tab := range tabs {
			if tab.Number == 0 {
				tab.Number = i + 1
			}
			tabByID[tab.ID] = tab
		}
	}

	for _, pane := range inv.Panes {
		if pane.Agent == "" {
			continue
		}
		if tabByID != nil {
			if tab, ok := tabByID[pane.TabID]; ok {
				pane.TabLabel = tab.Label
				pane.TabNumber = tab.Number
			}
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

	if p.enrich != nil {
		p.enrich(ctx, agents)
	}

	if !p.state.CommitPoll(agents, token) {
		p.logger.Debug("discarded topology-stale inventory sample")
		p.handleTopologyStale(previousStatus)
		return
	}
	p.topologyRetries = 0
	p.notifyStatusChange(previousStatus)
	p.logger.Debug("inventory committed", "agents", len(agents), "topology", p.state.TopologyGeneration())

	if p.onChange != nil {
		p.onChange(p.state.Snapshot())
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

func (p *Poller) currentInterval() time.Duration {
	if p.pollFailures >= pollFailureBackoffAfter {
		return idlePollInterval
	}
	if p.state.AgentCount() == 0 {
		return idlePollInterval
	}
	snap := p.state.Snapshot()
	allIdle := true
	for _, a := range snap {
		if attentionStatuses[a.Status] {
			allIdle = false
			break
		}
	}
	if allIdle {
		return idlePollInterval
	}
	return p.interval
}
