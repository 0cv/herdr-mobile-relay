package hc

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/slashcmd"
)

type terminal struct {
	Address    string            `json:"address"`
	PID        *int              `json:"pid"`
	State      string            `json:"state"`
	Labels     map[string]string `json:"labels"`
	Name       string            `json:"name"`
	Title      string            `json:"title"`
	Cwd        string            `json:"cwd"`
	Argv       []string          `json:"argv"`
	Reported   bool              `json:"reported"`
	Turn       int64             `json:"turn"`
	Status     *terminalStatus   `json:"status"`
	Foreground *foreground       `json:"foreground"`
}

type terminalStatus struct {
	State string `json:"state"`
	App   string `json:"app"`
}

type foreground struct {
	PID  int      `json:"pid"`
	Name string   `json:"name"`
	Cwd  string   `json:"cwd"`
	Argv []string `json:"argv"`
}

func (c *Client) bumpEpoch(address string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	epoch, ok := c.epochs[address]
	if !ok {
		epoch = c.epochFloor
	}
	c.epochs[address] = epoch + 1
}

func (c *Client) bumpAllEpochs() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, epoch := range c.epochs {
		if epoch > c.epochFloor {
			c.epochFloor = epoch
		}
	}
	c.epochFloor++
	for address := range c.epochs {
		c.epochs[address] = c.epochFloor
	}
}

func (c *Client) setRevision(address string, seq int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.revisions[address] = seq
}

func (t terminal) listed() bool {
	return t.State == "live" && t.PID != nil && !strings.Contains(t.Address, "/")
}

func (c *Client) pane(t terminal) herdr.Pane {
	c.mu.Lock()
	epoch, ok := c.epochs[t.Address]
	if !ok {
		epoch = c.epochFloor
		c.epochs[t.Address] = epoch
	}
	revision := c.revisions[t.Address]
	c.mu.Unlock()
	p := herdr.Pane{
		ID: t.Address, TerminalID: fmt.Sprintf("%s:%d:%d", t.Address, *t.PID, epoch),
		WorkspaceID: t.Labels["herdr.space"], TabID: t.Labels["herdr.tab"],
		Name: t.Name, Status: "unknown", Cwd: t.Cwd, Revision: revision, StateChangeSeq: t.Turn,
	}
	if p.WorkspaceID == "" {
		p.WorkspaceID = "core"
	}
	if p.TabID == "" {
		p.TabID = "core"
	}
	if p.Name == "" {
		p.Name = t.Title
	}
	if t.Status != nil {
		p.Status = t.Status.State
		if p.Status == "error" {
			p.Status = "done"
		}
		if t.Reported {
			p.Agent = t.Status.App
		}
	}
	if t.Foreground != nil {
		p.ForegroundCwd = t.Foreground.Cwd
		if p.Agent == "" && slashcmd.ProfileIDForAgentName(t.Foreground.Name) != "" {
			p.Agent = t.Foreground.Name
		}
	}
	return p
}

func (c *Client) PaneList(ctx context.Context) ([]herdr.Pane, error) {
	raw, err := c.call(ctx, "terminal.list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var result struct {
		Terminals []terminal `json:"terminals"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	panes := make([]herdr.Pane, 0, len(result.Terminals))
	for _, t := range result.Terminals {
		if t.listed() {
			panes = append(panes, c.pane(t))
		}
	}
	return panes, nil
}

func (c *Client) GetInventory(ctx context.Context) (*herdr.Inventory, error) {
	panes, err := c.PaneList(ctx)
	if err != nil {
		return nil, err
	}
	inventory := &herdr.Inventory{Panes: make([]herdr.Pane, 0, len(panes))}
	for _, p := range panes {
		if p.Agent != "" {
			inventory.Panes = append(inventory.Panes, p)
		}
	}
	return inventory, nil
}

func (c *Client) getTerminal(ctx context.Context, paneID string) (*terminal, error) {
	if strings.Contains(paneID, "/") {
		return nil, &herdr.CLIError{Code: "pane_not_found"}
	}
	raw, err := c.call(ctx, "terminal.get", map[string]any{"terminal": paneID})
	if err != nil {
		return nil, err
	}
	var t terminal
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, err
	}
	if !t.listed() {
		return nil, &herdr.CLIError{Code: "pane_not_found"}
	}
	return &t, nil
}

func (c *Client) PaneGet(ctx context.Context, paneID string) (*herdr.Pane, error) {
	t, err := c.getTerminal(ctx, paneID)
	if err != nil {
		return nil, err
	}
	p := c.pane(*t)
	return &p, nil
}

func (c *Client) PaneProcessInfoFresh(ctx context.Context, paneID string) (*herdr.PaneProcessInfo, error) {
	t, err := c.getTerminal(ctx, paneID)
	if err != nil {
		return nil, err
	}
	info := &herdr.PaneProcessInfo{PaneID: t.Address}
	if len(t.Argv) > 0 {
		switch strings.TrimPrefix(filepath.Base(t.Argv[0]), "-") {
		case "zsh", "bash", "fish", "sh", "dash", "nu", "pwsh":
			info.ShellPID = *t.PID
		}
	}
	if f := t.Foreground; f != nil {
		info.ForegroundProcessGroupID = f.PID
		info.ForegroundProcesses = []herdr.PaneProcess{{PID: f.PID, Name: f.Name, Cwd: f.Cwd, Argv: f.Argv}}
	}
	return info, nil
}

func (c *Client) topology(ctx context.Context) ([]herdr.Workspace, []herdr.Tab, []herdr.Pane, error) {
	raw, err := c.call(ctx, "doc.get", map[string]any{"name": "herdr-app"})
	if err != nil && !herdr.IsPaneNotFound(err) {
		return nil, nil, nil, err
	}
	var doc struct {
		Value struct {
			Spaces []struct {
				ID     string `json:"id"`
				Name   string `json:"name"`
				Folder string `json:"folder"`
				Tabs   []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"tabs"`
			} `json:"spaces"`
		} `json:"value"`
	}
	if err == nil {
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, nil, nil, err
		}
	}
	panes, err := c.PaneList(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	spaces := make([]herdr.Workspace, 0)
	tabs := make([]herdr.Tab, 0)
	spaceIndex := make(map[string]int)
	tabIndex := make(map[[2]string]int)
	addSpace := func(id, name, cwd string) int {
		if i, ok := spaceIndex[id]; ok {
			return i
		}
		i := len(spaces)
		spaceIndex[id] = i
		spaces = append(spaces, herdr.Workspace{ID: id, Label: name, Cwd: cwd, Number: i + 1})
		return i
	}
	addTab := func(space int, id, name, cwd string) {
		key := [2]string{spaces[space].ID, id}
		if _, ok := tabIndex[key]; ok {
			return
		}
		tabIndex[key] = len(tabs)
		spaces[space].TabCount++
		tabs = append(tabs, herdr.Tab{ID: id, WorkspaceID: key[0], Label: name, Cwd: cwd, Number: spaces[space].TabCount})
	}
	for _, s := range doc.Value.Spaces {
		i := addSpace(s.ID, s.Name, s.Folder)
		for _, t := range s.Tabs {
			addTab(i, t.ID, t.Name, s.Folder)
		}
	}
	for _, p := range panes {
		i := addSpace(p.WorkspaceID, p.WorkspaceID, p.Cwd)
		spaces[i].PaneCount++
		addTab(i, p.TabID, p.TabID, p.Cwd)
	}
	return spaces, tabs, panes, nil
}

func (c *Client) WorkspaceList(ctx context.Context) ([]herdr.Workspace, error) {
	spaces, _, _, err := c.topology(ctx)
	return spaces, err
}

func (c *Client) TabList(ctx context.Context) ([]herdr.Tab, error) {
	_, tabs, _, err := c.topology(ctx)
	return tabs, err
}
