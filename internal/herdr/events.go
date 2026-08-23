package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"sync"
	"time"
)

const eventSubscriptionRequestID = "mobile-relay-events"

type Event struct {
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data"`
}

var eventNameAliases = map[string]string{
	"workspace_created":          "workspace.created",
	"workspace_updated":          "workspace.updated",
	"workspace_metadata_updated": "workspace.metadata_updated",
	"workspace_closed":           "workspace.closed",
	"workspace_renamed":          "workspace.renamed",
	"workspace_moved":            "workspace.moved",
	"workspace_reordered":        "workspace.reordered",
	"workspace_focused":          "workspace.focused",
	"worktree_created":           "worktree.created",
	"worktree_opened":            "worktree.opened",
	"worktree_removed":           "worktree.removed",
	"tab_created":                "tab.created",
	"tab_closed":                 "tab.closed",
	"tab_renamed":                "tab.renamed",
	"tab_moved":                  "tab.moved",
	"tab_focused":                "tab.focused",
	"pane_created":               "pane.created",
	"pane_closed":                "pane.closed",
	"pane_updated":               "pane.updated",
	"pane_focused":               "pane.focused",
	"pane_moved":                 "pane.moved",
	"pane_output_changed":        "pane.output_changed",
	"pane_exited":                "pane.exited",
	"pane_agent_detected":        "pane.agent_detected",
	"pane_agent_status_changed":  "pane.agent_status_changed",
	"layout_updated":             "layout.updated",
}

func (e *Event) UnmarshalJSON(data []byte) error {
	var envelope struct {
		Event string          `json:"event"`
		Data  json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	if canonical, ok := eventNameAliases[envelope.Event]; ok {
		envelope.Event = canonical
	}
	e.Event = envelope.Event
	e.Data = envelope.Data
	return nil
}

type SessionSnapshot struct {
	Version          string          `json:"version"`
	Protocol         int             `json:"protocol"`
	Workspaces       []Workspace     `json:"workspaces"`
	Tabs             []Tab           `json:"tabs"`
	Panes            []SnapshotPane  `json:"panes"`
	Agents           []SnapshotAgent `json:"agents"`
	FocusedWorkspace string          `json:"focused_workspace_id"`
	FocusedTab       string          `json:"focused_tab_id"`
	FocusedPane      string          `json:"focused_pane_id"`
}

type SnapshotPane struct {
	ID            string `json:"pane_id"`
	TerminalID    string `json:"terminal_id"`
	WorkspaceID   string `json:"workspace_id"`
	TabID         string `json:"tab_id"`
	Focused       bool   `json:"focused"`
	Cwd           string `json:"cwd"`
	ForegroundCwd string `json:"foreground_cwd"`
	Label         string `json:"label"`
	Agent         string `json:"agent"`
	Status        string `json:"agent_status"`
	Revision      int    `json:"revision"`
	Scroll        struct {
		MaxOffsetFromBottom int `json:"max_offset_from_bottom"`
	} `json:"scroll"`
}

type SnapshotAgent struct {
	PaneID         string `json:"pane_id"`
	TerminalID     string `json:"terminal_id"`
	WorkspaceID    string `json:"workspace_id"`
	TabID          string `json:"tab_id"`
	Focused        bool   `json:"focused"`
	Name           string `json:"name"`
	Agent          string `json:"agent"`
	Status         string `json:"agent_status"`
	Cwd            string `json:"cwd"`
	ForegroundCwd  string `json:"foreground_cwd"`
	Revision       int    `json:"revision"`
	StateChangeSeq int64  `json:"state_change_seq"`
	AgentSession   struct {
		Value string `json:"value"`
		Kind  string `json:"kind"`
	} `json:"agent_session"`
}

type TopologySnapshot struct {
	Workspaces []Workspace
	Panes      []Pane
	Tabs       []Tab
}

type EventClient struct {
	path string
	// supportsWorkspaceReordered reports whether the running Herdr build
	// accepts a workspace.reordered subscription. Herdr 0.7.5 (the supported
	// minimum) rejects the whole events.subscribe request when the name is
	// unknown, degrading realtime updates to polling.
	supportsWorkspaceReordered func() bool
}

func NewEventClient(path string) *EventClient {
	return &EventClient{path: path}
}

// SetWorkspaceReorderedProbe installs the capability probe consulted on each
// subscribe. The probe runs lazily so constructing the client stays cheap;
// when unset, workspace.reordered is excluded.
func (c *EventClient) SetWorkspaceReorderedProbe(probe func() bool) {
	c.supportsWorkspaceReordered = probe
}

// Bootstrap subscribes before taking a snapshot. Events arriving while the
// snapshot is in flight remain queued and are returned for replay afterward.
func (c *EventClient) Bootstrap(ctx context.Context) (*EventStream, SessionSnapshot, []Event, error) {
	stream, err := c.subscribe(ctx)
	if err != nil {
		return nil, SessionSnapshot{}, nil, err
	}
	snapshot, err := c.snapshot(ctx)
	if err != nil {
		_ = stream.Close()
		return nil, SessionSnapshot{}, nil, err
	}
	return stream, snapshot, stream.drain(), nil
}

func (c *EventClient) subscribe(ctx context.Context) (*EventStream, error) {
	if c == nil || c.path == "" {
		return nil, errors.New("Herdr socket path is unavailable")
	}
	requestCtx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(requestCtx, "unix", c.path)
	if err != nil {
		return nil, fmt.Errorf("connect to Herdr events socket: %w", err)
	}
	stopWatch := closeOnContextDone(requestCtx, conn)
	defer stopWatch()
	if err := setSocketDeadline(conn, requestCtx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("set Herdr events socket deadline: %w", err)
	}
	request := map[string]any{
		"id":     eventSubscriptionRequestID,
		"method": "events.subscribe",
		"params": map[string]any{
			"subscriptions": topologySubscriptions(
				c.supportsWorkspaceReordered != nil && c.supportsWorkspaceReordered(),
			),
		},
	}
	if err := writeSocketJSON(conn, request); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("write Herdr events subscription: %w", err)
	}
	reader := bufio.NewReader(conn)
	line, err := readSocketAPILine(reader)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("read Herdr events subscription: %w", err)
	}
	var response struct {
		ID     string `json:"id"`
		Result struct {
			Type string `json:"type"`
		} `json:"result"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(line, &response); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("decode Herdr events subscription: %w", err)
	}
	if response.ID != eventSubscriptionRequestID {
		_ = conn.Close()
		return nil, errors.New("Herdr events subscription response ID mismatch")
	}
	if response.Error != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("Herdr events subscription %s: %s", response.Error.Code, response.Error.Message)
	}
	if response.Result.Type != "subscription_started" {
		_ = conn.Close()
		return nil, fmt.Errorf("Herdr events subscription returned %q", response.Result.Type)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("clear Herdr events socket deadline: %w", err)
	}
	stream := &EventStream{conn: conn, queue: newEventQueue()}
	go stream.readLoop(reader)
	return stream, nil
}

func (c *EventClient) snapshot(ctx context.Context) (SessionSnapshot, error) {
	if c == nil || c.path == "" {
		return SessionSnapshot{}, errors.New("Herdr socket path is unavailable")
	}
	requestCtx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(requestCtx, "unix", c.path)
	if err != nil {
		return SessionSnapshot{}, fmt.Errorf("connect to Herdr snapshot socket: %w", err)
	}
	defer conn.Close()
	stopWatch := closeOnContextDone(requestCtx, conn)
	defer stopWatch()
	if err := setSocketDeadline(conn, requestCtx); err != nil {
		return SessionSnapshot{}, fmt.Errorf("set Herdr snapshot socket deadline: %w", err)
	}
	const requestID = "mobile-relay-snapshot"
	request := map[string]any{"id": requestID, "method": "session.snapshot", "params": map[string]any{}}
	if err := writeSocketJSON(conn, request); err != nil {
		return SessionSnapshot{}, fmt.Errorf("write Herdr session snapshot: %w", err)
	}
	line, err := readSocketAPILine(bufio.NewReader(conn))
	if err != nil {
		return SessionSnapshot{}, fmt.Errorf("read Herdr session snapshot: %w", err)
	}
	var response struct {
		ID     string `json:"id"`
		Result struct {
			Type     string          `json:"type"`
			Snapshot SessionSnapshot `json:"snapshot"`
		} `json:"result"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(line, &response); err != nil {
		return SessionSnapshot{}, fmt.Errorf("decode Herdr session snapshot: %w", err)
	}
	if response.ID != requestID {
		return SessionSnapshot{}, errors.New("Herdr session snapshot response ID mismatch")
	}
	if response.Error != nil {
		return SessionSnapshot{}, fmt.Errorf("Herdr session snapshot %s: %s", response.Error.Code, response.Error.Message)
	}
	if response.Result.Type != "session_snapshot" {
		return SessionSnapshot{}, fmt.Errorf("Herdr session snapshot returned %q", response.Result.Type)
	}
	return response.Result.Snapshot, nil
}

// topologySubscriptions lists the events the relay needs for realtime
// topology. workspace.reordered exists only in Herdr builds that also expose
// workspace.move_block; older builds reject the entire events.subscribe
// request when the name is present, so it is gated on the capability probe.
func topologySubscriptions(includeWorkspaceReordered bool) []map[string]string {
	names := []string{
		"pane.created",
		"pane.closed",
		"pane.updated",
		"pane.moved",
		"pane.exited",
		"pane.agent_detected",
		"tab.created",
		"tab.closed",
		"tab.renamed",
		"tab.moved",
		"workspace.created",
		"workspace.updated",
		"workspace.metadata_updated",
		"workspace.closed",
		"workspace.renamed",
		"workspace.moved",
		"workspace.focused",
		"worktree.created",
		"worktree.opened",
		"worktree.removed",
	}
	if includeWorkspaceReordered {
		names = append(names, "workspace.reordered")
	}
	subscriptions := make([]map[string]string, len(names))
	for index, name := range names {
		subscriptions[index] = map[string]string{"type": name}
	}
	return subscriptions
}

// closeOnContextDone closes conn when ctx finishes, so a peer that accepts a
// request and then stops responding cannot keep a read blocked until the
// socket deadline and delay shutdown. The returned func ends the watcher and
// must run before the request context is cancelled.
func closeOnContextDone(ctx context.Context, conn net.Conn) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	return func() { close(done) }
}

func setSocketDeadline(conn net.Conn, ctx context.Context) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(defaultTimeout)
	}
	return conn.SetDeadline(deadline)
}

func writeSocketJSON(conn net.Conn, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	_, err = conn.Write(payload)
	return err
}

type EventStream struct {
	conn      net.Conn
	queue     *eventQueue
	closeOnce sync.Once
}

func (s *EventStream) Next(ctx context.Context) (Event, error) {
	if s == nil || s.queue == nil {
		return Event{}, errors.New("Herdr event stream is unavailable")
	}
	return s.queue.next(ctx)
}

func (s *EventStream) Close() error {
	if s == nil {
		return nil
	}
	var err error
	s.closeOnce.Do(func() {
		err = s.conn.Close()
		s.queue.close(io.EOF)
	})
	return err
}

func (s *EventStream) drain() []Event {
	return s.queue.drain()
}

func (s *EventStream) readLoop(reader *bufio.Reader) {
	for {
		line, err := readSocketAPILine(reader)
		if err != nil {
			s.queue.close(err)
			return
		}
		var event Event
		if err := json.Unmarshal(line, &event); err != nil {
			s.queue.close(fmt.Errorf("decode Herdr event: %w", err))
			return
		}
		if event.Event == "" {
			s.queue.close(errors.New("Herdr event has no event kind"))
			return
		}
		s.queue.push(event)
	}
}

type eventQueue struct {
	mu     sync.Mutex
	items  []Event
	closed bool
	err    error
	notify chan struct{}
}

func newEventQueue() *eventQueue {
	return &eventQueue{notify: make(chan struct{})}
}

func (q *eventQueue) push(event Event) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.items = append(q.items, event)
	close(q.notify)
	q.notify = make(chan struct{})
}

func (q *eventQueue) close(err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.closed = true
	q.err = err
	close(q.notify)
}

func (q *eventQueue) next(ctx context.Context) (Event, error) {
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			event := q.items[0]
			q.items = q.items[1:]
			q.mu.Unlock()
			return event, nil
		}
		if q.closed {
			err := q.err
			q.mu.Unlock()
			if err == nil {
				err = io.EOF
			}
			return Event{}, err
		}
		notify := q.notify
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return Event{}, ctx.Err()
		case <-notify:
		}
	}
}

func (q *eventQueue) drain() []Event {
	q.mu.Lock()
	defer q.mu.Unlock()
	items := append([]Event(nil), q.items...)
	q.items = nil
	return items
}

type SessionCache struct {
	workspaces     map[string]Workspace
	workspaceOrder []string
	panes          map[string]Pane
	tabs           map[string]Tab
	// tabOrder holds tab IDs in Herdr's visual order. Numbers are stable
	// identities that never change when a tab moves, so array order is the
	// only truth for on-screen position.
	tabOrder []string
}

func NewSessionCache(snapshot SessionSnapshot) *SessionCache {
	cache := &SessionCache{
		workspaces: make(map[string]Workspace, len(snapshot.Workspaces)),
		panes:      make(map[string]Pane, len(snapshot.Panes)),
		tabs:       make(map[string]Tab, len(snapshot.Tabs)),
	}
	for _, workspace := range snapshot.Workspaces {
		cache.setWorkspace(workspace)
	}
	for _, tab := range snapshot.Tabs {
		cache.setTab(tab)
	}
	for _, pane := range snapshot.Panes {
		if pane.ID != "" {
			cache.panes[pane.ID] = paneToInventoryPane(pane)
		}
	}
	for _, agent := range snapshot.Agents {
		cache.applyAgent(agent)
	}
	return cache
}

func (c *SessionCache) Snapshot() TopologySnapshot {
	// Tab and pane events update only the tab and pane maps, so counts copied
	// verbatim from cached workspace records go stale until the next
	// reconcile poll. Derive them from the maps so every snapshot is
	// internally consistent.
	tabCounts := make(map[string]int, len(c.workspaces))
	for _, tab := range c.tabs {
		tabCounts[tab.WorkspaceID]++
	}
	paneCounts := make(map[string]int, len(c.workspaces))
	for _, pane := range c.panes {
		paneCounts[pane.WorkspaceID]++
	}
	workspaces := make([]Workspace, 0, len(c.workspaces))
	for _, id := range c.workspaceOrder {
		if workspace, ok := c.workspaces[id]; ok {
			if workspace.Cwd == "" {
				workspace.Cwd = c.workspaceCwd(id, workspace)
			}
			workspace.TabCount = tabCounts[id]
			workspace.PaneCount = paneCounts[id]
			if workspace.ActiveTabID != "" {
				// The successor of a closed active tab cannot be derived
				// locally; keep the authoritative value otherwise, but never
				// report a closed tab as active.
				if _, open := c.tabs[workspace.ActiveTabID]; !open {
					workspace.ActiveTabID = ""
				}
			}
			workspaces = append(workspaces, workspace)
		}
	}
	panes := make([]Pane, 0, len(c.panes))
	for _, pane := range c.panes {
		panes = append(panes, pane)
	}
	sort.Slice(panes, func(i, j int) bool { return panes[i].ID < panes[j].ID })
	tabs := make([]Tab, 0, len(c.tabs))
	for _, id := range c.tabOrder {
		if tab, ok := c.tabs[id]; ok {
			tabs = append(tabs, tab)
		}
	}
	return TopologySnapshot{Workspaces: workspaces, Panes: panes, Tabs: tabs}
}

func (c *SessionCache) Apply(event Event) (bool, error) {
	switch event.Event {
	case "pane.created", "pane.updated":
		var data struct {
			Pane SnapshotPane `json:"pane"`
		}
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return false, err
		}
		if data.Pane.ID == "" {
			return false, nil
		}
		return c.setPane(data.Pane), nil
	case "pane.moved":
		var data struct {
			PreviousPaneID    string       `json:"previous_pane_id"`
			Pane              SnapshotPane `json:"pane"`
			ClosedWorkspaceID string       `json:"closed_workspace_id"`
			ClosedTabID       string       `json:"closed_tab_id"`
		}
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return false, err
		}
		if data.PreviousPaneID != "" && data.PreviousPaneID != data.Pane.ID {
			delete(c.panes, data.PreviousPaneID)
		}
		if data.Pane.ID != "" {
			c.setPane(data.Pane)
		}
		c.removeTab(data.ClosedTabID)
		c.removeWorkspace(data.ClosedWorkspaceID)
		return true, nil
	case "pane.closed":
		var data struct {
			PaneID string `json:"pane_id"`
		}
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return false, err
		}
		if data.PaneID == "" {
			return false, nil
		}
		delete(c.panes, data.PaneID)
		return true, nil
	case "pane.exited":
		var data struct {
			PaneID string `json:"pane_id"`
		}
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return false, err
		}
		delete(c.panes, data.PaneID)
		return data.PaneID != "", nil
	case "pane.agent_detected":
		var data struct {
			PaneID      string  `json:"pane_id"`
			Agent       *string `json:"agent"`
			Released    bool    `json:"released"`
			FinalStatus string  `json:"final_status"`
		}
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return false, err
		}
		pane, ok := c.panes[data.PaneID]
		if !ok {
			return false, nil
		}
		changed := false
		if data.Released || data.Agent == nil {
			if pane.Agent != "" || pane.Name != "" || pane.Session != "" || pane.SessionRaw.Value != "" || pane.SessionRaw.Kind != "" {
				changed = true
			}
			pane.Agent = ""
			pane.Name = ""
			pane.Session = ""
			pane.SessionRaw.Value = ""
			pane.SessionRaw.Kind = ""
		} else if pane.Agent != *data.Agent {
			pane.Agent = *data.Agent
			changed = true
		}
		if data.FinalStatus != "" && pane.Status != data.FinalStatus {
			pane.Status = data.FinalStatus
			changed = true
		}
		if !changed {
			return false, nil
		}
		c.panes[data.PaneID] = pane
		return true, nil
	case "tab.created", "tab.renamed":
		var data struct {
			Tab         Tab    `json:"tab"`
			TabID       string `json:"tab_id"`
			WorkspaceID string `json:"workspace_id"`
			Label       string `json:"label"`
			Number      int    `json:"number"`
			Cwd         string `json:"cwd"`
		}
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return false, err
		}
		if data.Tab.ID != "" {
			c.setTab(data.Tab)
			return true, nil
		}
		if data.TabID == "" {
			return false, nil
		}
		c.setTab(Tab{
			ID:          data.TabID,
			WorkspaceID: data.WorkspaceID,
			Label:       data.Label,
			Number:      data.Number,
			Cwd:         data.Cwd,
		})
		return true, nil
	case "tab.moved":
		var data struct {
			Tab         Tab    `json:"tab"`
			TabID       string `json:"tab_id"`
			Tabs        []Tab  `json:"tabs"`
			WorkspaceID string `json:"workspace_id"`
			Number      int    `json:"number"`
		}
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return false, err
		}
		changed := false
		if data.Tab.ID != "" {
			c.setTab(data.Tab)
			changed = true
		}
		for _, tab := range data.Tabs {
			c.setTab(tab)
		}
		if len(data.Tabs) > 0 {
			c.reorderTabs(data.Tabs)
			changed = true
		}
		if data.TabID != "" {
			c.setTab(Tab{
				ID:          data.TabID,
				WorkspaceID: data.WorkspaceID,
				Number:      data.Number,
			})
			changed = true
		}
		return changed, nil
	case "tab.closed":
		var data struct {
			TabID string `json:"tab_id"`
		}
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return false, err
		}
		c.removeTab(data.TabID)
		return data.TabID != "", nil
	case "workspace.closed", "worktree.removed":
		var data struct {
			WorkspaceID string `json:"workspace_id"`
		}
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return false, err
		}
		return c.removeWorkspace(data.WorkspaceID), nil
	case "workspace.created", "workspace.updated", "workspace.metadata_updated",
		"worktree.created", "worktree.opened":
		var data struct {
			Workspace Workspace `json:"workspace"`
		}
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return false, err
		}
		return c.setWorkspace(data.Workspace), nil
	case "workspace.renamed":
		var data struct {
			Workspace   Workspace `json:"workspace"`
			WorkspaceID string    `json:"workspace_id"`
			Label       string    `json:"label"`
		}
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return false, err
		}
		if data.Workspace.ID != "" {
			return c.setWorkspace(data.Workspace), nil
		}
		workspace := c.workspaces[data.WorkspaceID]
		workspace.ID = data.WorkspaceID
		workspace.Label = data.Label
		return c.setWorkspace(workspace), nil
	case "workspace.focused":
		var data struct {
			WorkspaceID string `json:"workspace_id"`
		}
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return false, err
		}
		changed := false
		for id, workspace := range c.workspaces {
			focused := id == data.WorkspaceID
			if workspace.Focused != focused {
				workspace.Focused = focused
				c.workspaces[id] = workspace
				changed = true
			}
		}
		return changed, nil
	case "workspace.moved", "workspace.reordered":
		var data struct {
			Workspaces []Workspace `json:"workspaces"`
		}
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return false, err
		}
		for _, workspace := range data.Workspaces {
			c.setWorkspace(workspace)
		}
		if len(data.Workspaces) > 0 {
			c.reorderWorkspaces(data.Workspaces)
			return true, nil
		}
		return false, nil
	default:
		return false, nil
	}
}

func (c *SessionCache) setPane(snapshot SnapshotPane) bool {
	previous, exists := c.panes[snapshot.ID]
	if exists && previous.Revision > 0 && snapshot.Revision > 0 && snapshot.Revision < previous.Revision {
		return false
	}
	pane := paneToInventoryPane(snapshot)
	if exists {
		pane.Agent = previous.Agent
		pane.Name = firstNonEmpty(pane.Name, previous.Name)
		pane.Session = firstNonEmpty(pane.Session, previous.Session)
		pane.SessionRaw = previous.SessionRaw
		if pane.StateChangeSeq == 0 {
			pane.StateChangeSeq = previous.StateChangeSeq
		}
	}
	c.panes[snapshot.ID] = pane
	return !exists || paneTopologyChanged(previous, pane)
}

// paneTopologyChanged excludes terminal-local churn from topology commits.
// Revision, status, scroll position, state sequence, and the pane label can
// change during ordinary output without changing the agent topology.
func paneTopologyChanged(previous, current Pane) bool {
	return previous.ID != current.ID ||
		previous.TerminalID != current.TerminalID ||
		previous.WorkspaceID != current.WorkspaceID ||
		previous.TabID != current.TabID ||
		previous.Focused != current.Focused ||
		previous.Cwd != current.Cwd ||
		previous.ForegroundCwd != current.ForegroundCwd ||
		previous.Agent != current.Agent
}

func (c *SessionCache) applyAgent(agent SnapshotAgent) {
	pane := c.panes[agent.PaneID]
	pane.ID = firstNonEmpty(agent.PaneID, pane.ID)
	pane.TerminalID = firstNonEmpty(agent.TerminalID, pane.TerminalID)
	pane.WorkspaceID = firstNonEmpty(agent.WorkspaceID, pane.WorkspaceID)
	pane.TabID = firstNonEmpty(agent.TabID, pane.TabID)
	pane.Focused = agent.Focused
	pane.Cwd = firstNonEmpty(agent.Cwd, pane.Cwd)
	pane.ForegroundCwd = firstNonEmpty(agent.ForegroundCwd, pane.ForegroundCwd)
	pane.Agent = agent.Agent
	pane.Name = agent.Name
	pane.Status = agent.Status
	pane.Revision = agent.Revision
	pane.StateChangeSeq = agent.StateChangeSeq
	pane.Session = agent.AgentSession.Value
	pane.SessionRaw.Value = agent.AgentSession.Value
	pane.SessionRaw.Kind = agent.AgentSession.Kind
	c.panes[pane.ID] = pane
}

// Pane snapshots describe every terminal, including terminals where a detected
// agent has already exited. Agent snapshots and pane.agent_detected events are
// the authoritative sources for active agent identity.
func paneToInventoryPane(snapshot SnapshotPane) Pane {
	return Pane{
		ID:          snapshot.ID,
		TerminalID:  snapshot.TerminalID,
		TabID:       snapshot.TabID,
		WorkspaceID: snapshot.WorkspaceID,
		Name:        snapshot.Label,
		Status:      snapshot.Status,
		Focused:     snapshot.Focused,
		Cwd:         snapshot.Cwd,
		Revision:    snapshot.Revision,
		Scroll: struct {
			MaxOffsetFromBottom int `json:"max_offset_from_bottom"`
		}{MaxOffsetFromBottom: snapshot.Scroll.MaxOffsetFromBottom},
		ForegroundCwd: snapshot.ForegroundCwd,
	}
}

func (c *SessionCache) removeTab(tabID string) {
	if tabID == "" {
		return
	}
	delete(c.tabs, tabID)
	c.pruneTabOrder()
	for paneID, pane := range c.panes {
		if pane.TabID == tabID {
			delete(c.panes, paneID)
		}
	}
}

func (c *SessionCache) removeWorkspace(workspaceID string) bool {
	if workspaceID == "" {
		return false
	}
	_, changed := c.workspaces[workspaceID]
	delete(c.workspaces, workspaceID)
	c.pruneWorkspaceOrder()
	for tabID, tab := range c.tabs {
		if tab.WorkspaceID == workspaceID {
			delete(c.tabs, tabID)
			changed = true
		}
	}
	c.pruneTabOrder()
	for paneID, pane := range c.panes {
		if pane.WorkspaceID == workspaceID {
			delete(c.panes, paneID)
			changed = true
		}
	}
	return changed
}

func (c *SessionCache) setWorkspace(workspace Workspace) bool {
	if workspace.ID == "" {
		return false
	}
	previous, exists := c.workspaces[workspace.ID]
	if exists {
		if workspace.Number == 0 {
			workspace.Number = previous.Number
		}
		if workspace.Label == "" {
			workspace.Label = previous.Label
		}
		if workspace.ActiveTabID == "" {
			workspace.ActiveTabID = previous.ActiveTabID
		}
		if workspace.AgentStatus == "" {
			workspace.AgentStatus = previous.AgentStatus
		}
		if workspace.Cwd == "" {
			workspace.Cwd = previous.Cwd
		}
		if workspace.Worktree == nil {
			workspace.Worktree = previous.Worktree
		}
	}
	c.workspaces[workspace.ID] = workspace
	if !exists {
		c.workspaceOrder = append(c.workspaceOrder, workspace.ID)
	}
	return !exists || !workspaceEqual(previous, workspace)
}

func workspaceEqual(left, right Workspace) bool {
	if left.ID != right.ID || left.Number != right.Number || left.Label != right.Label ||
		left.Focused != right.Focused || left.PaneCount != right.PaneCount ||
		left.TabCount != right.TabCount || left.ActiveTabID != right.ActiveTabID ||
		left.AgentStatus != right.AgentStatus || left.Cwd != right.Cwd {
		return false
	}
	if left.Worktree == nil || right.Worktree == nil {
		return left.Worktree == nil && right.Worktree == nil
	}
	return *left.Worktree == *right.Worktree
}

func (c *SessionCache) workspaceCwd(workspaceID string, workspace Workspace) string {
	if workspace.Worktree != nil && workspace.Worktree.CheckoutPath != "" {
		return workspace.Worktree.CheckoutPath
	}
	cwd := ""
	for _, tab := range c.tabs {
		if tab.WorkspaceID != workspaceID || tab.Cwd == "" {
			continue
		}
		if cwd == "" || len(tab.Cwd) < len(cwd) {
			cwd = tab.Cwd
		}
	}
	for _, pane := range c.panes {
		if pane.WorkspaceID != workspaceID || pane.Cwd == "" {
			continue
		}
		if cwd == "" || len(pane.Cwd) < len(cwd) {
			cwd = pane.Cwd
		}
	}
	return cwd
}

func (c *SessionCache) reorderWorkspaces(ordered []Workspace) {
	seen := make(map[string]bool, len(ordered))
	next := make([]string, 0, len(c.workspaceOrder))
	for _, workspace := range ordered {
		if workspace.ID != "" {
			seen[workspace.ID] = true
			next = append(next, workspace.ID)
		}
	}
	for _, id := range c.workspaceOrder {
		if !seen[id] {
			next = append(next, id)
		}
	}
	c.workspaceOrder = next
}

func (c *SessionCache) pruneWorkspaceOrder() {
	kept := c.workspaceOrder[:0]
	for _, id := range c.workspaceOrder {
		if _, ok := c.workspaces[id]; ok {
			kept = append(kept, id)
		}
	}
	c.workspaceOrder = kept
}

// setTab upserts tab metadata, keeping previously known fields that the
// incoming payload omits, and registers unseen tabs at the end of the order.
func (c *SessionCache) setTab(tab Tab) {
	if tab.ID == "" {
		return
	}
	previous, exists := c.tabs[tab.ID]
	if exists {
		if tab.WorkspaceID == "" {
			tab.WorkspaceID = previous.WorkspaceID
		}
		if tab.Label == "" {
			tab.Label = previous.Label
		}
		if tab.Number == 0 {
			tab.Number = previous.Number
		}
		if tab.Cwd == "" {
			tab.Cwd = previous.Cwd
		}
	}
	c.tabs[tab.ID] = tab
	if !exists {
		c.tabOrder = append(c.tabOrder, tab.ID)
	}
}

// reorderTabs applies the authoritative post-move order from a tab.moved
// event. The event lists one workspace's tabs; tabs from other workspaces
// keep their relative order, which is all downstream grouping depends on.
func (c *SessionCache) reorderTabs(ordered []Tab) {
	moved := make(map[string]bool, len(ordered))
	for _, tab := range ordered {
		if tab.ID != "" {
			moved[tab.ID] = true
		}
	}
	next := make([]string, 0, len(c.tabOrder))
	for _, id := range c.tabOrder {
		if !moved[id] {
			next = append(next, id)
		}
	}
	for _, tab := range ordered {
		if tab.ID != "" {
			next = append(next, tab.ID)
		}
	}
	c.tabOrder = next
}

func (c *SessionCache) pruneTabOrder() {
	kept := c.tabOrder[:0]
	for _, id := range c.tabOrder {
		if _, ok := c.tabs[id]; ok {
			kept = append(kept, id)
		}
	}
	c.tabOrder = kept
}
