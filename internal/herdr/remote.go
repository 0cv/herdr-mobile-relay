package herdr

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

const remotePrefix = "remote/"

// IsRemoteID also recognizes malformed remote IDs: never fall back to local.
func IsRemoteID(id string) bool { return strings.HasPrefix(id, remotePrefix) }

func remoteID(machine, id string) string {
	if id == "" {
		return ""
	}
	return remotePrefix + machine + "/" + id
}

type RemoteMachine struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Enabled bool   `json:"enabled"`
}

// RemoteInventory is an immutable snapshot owned by Client. Consumers must not
// mutate its slices. Failed/disabled machines are removed on every refresh.
type RemoteInventory struct {
	Panes      []Pane
	Workspaces []Workspace
	Tabs       []Tab
}

func (c *Client) RemoteInventory() RemoteInventory {
	c.remoteMu.RLock()
	defer c.remoteMu.RUnlock()
	return c.remoteInventory
}

// RefreshRemoteInventory uses Herdr's saved-machine transport, not a second SSH
// implementation. Call outside the local inventory/event loop.
func (c *Client) RefreshRemoteInventory(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, 8*time.Second)
	defer cancel()
	out, err := c.runCommand(ctx, "machine", "list", "--json")
	var machines []RemoteMachine
	if err == nil {
		err = json.Unmarshal(out, &machines)
	}
	if err != nil {
		c.setRemoteInventory(RemoteInventory{})
		return fmt.Errorf("saved machine discovery: %w", err)
	}
	snapshots := make([]RemoteInventory, len(machines))
	failures := make([]error, len(machines))
	slots := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for index, machine := range machines {
		if !machine.Enabled {
			continue
		}
		if !validMachineID(machine.ID) {
			failures[index] = errors.New("invalid saved machine ID")
			continue
		}
		wg.Go(func() {
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				failures[index] = ctx.Err()
				return
			}
			machineCtx, stop := context.WithTimeout(ctx, 5*time.Second)
			defer stop()
			snapshots[index], failures[index] = c.readRemoteInventory(machineCtx, machine)
		})
	}
	wg.Wait()
	combined := RemoteInventory{}
	for _, snapshot := range snapshots {
		combined.Panes = append(combined.Panes, snapshot.Panes...)
		combined.Workspaces = append(combined.Workspaces, snapshot.Workspaces...)
		combined.Tabs = append(combined.Tabs, snapshot.Tabs...)
	}
	c.setRemoteInventory(combined)
	return errors.Join(failures...)
}

func validMachineID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func (c *Client) setRemoteInventory(inventory RemoteInventory) {
	c.remoteMu.Lock()
	defer c.remoteMu.Unlock()
	c.remoteInventory = inventory
}

func (c *Client) readRemoteInventory(ctx context.Context, machine RemoteMachine) (RemoteInventory, error) {
	var agents struct {
		Agents *[]Pane `json:"agents"`
	}
	var workspaces struct {
		Workspaces *[]Workspace `json:"workspaces"`
	}
	var tabs struct {
		Tabs *[]Tab `json:"tabs"`
	}
	for _, query := range []struct {
		command string
		result  any
	}{
		{"agent", &agents}, {"workspace", &workspaces}, {"tab", &tabs},
	} {
		if err := c.runResult(ctx, query.result, "--machine", machine.ID, query.command, "list"); err != nil {
			return RemoteInventory{}, fmt.Errorf("remote machine %s inventory: %w", machine.ID, err)
		}
	}
	if agents.Agents == nil || workspaces.Workspaces == nil || tabs.Tabs == nil {
		return RemoteInventory{}, fmt.Errorf("remote machine %s: incomplete inventory", machine.ID)
	}
	snapshot := RemoteInventory{Panes: *agents.Agents, Workspaces: *workspaces.Workspaces, Tabs: *tabs.Tabs}
	for i := range snapshot.Panes {
		pane := &snapshot.Panes[i]
		pane.ID = remoteID(machine.ID, pane.ID)
		pane.TerminalID = remoteID(machine.ID, pane.TerminalID)
		pane.WorkspaceID = remoteID(machine.ID, pane.WorkspaceID)
		pane.TabID = remoteID(machine.ID, pane.TabID)
		pane.MachineID, pane.MachineLabel = machine.ID, machine.Label
		// Session paths and IDs are remote; never hand them to local transcript discovery.
		pane.Session = ""
	}
	for i := range snapshot.Workspaces {
		workspace := &snapshot.Workspaces[i]
		workspace.ID = remoteID(machine.ID, workspace.ID)
		workspace.ActiveTabID = remoteID(machine.ID, workspace.ActiveTabID)
		workspace.MachineID, workspace.MachineLabel, workspace.ReadOnly = machine.ID, machine.Label, true
		workspace.Label = machine.Label + " · " + workspace.Label
		workspace.Worktree = nil // Local worktree/filesystem operations do not apply.
	}
	for i := range snapshot.Tabs {
		tab := &snapshot.Tabs[i]
		tab.ID = remoteID(machine.ID, tab.ID)
		tab.WorkspaceID = remoteID(machine.ID, tab.WorkspaceID)
	}
	return snapshot, nil
}

func (c *Client) readRemotePane(ctx context.Context, paneID string, lines int, format, source string) (PaneRead, error) {
	var machineID, rawID string
	for _, pane := range c.RemoteInventory().Panes {
		if pane.ID == paneID {
			machineID = pane.MachineID
			rawID = strings.TrimPrefix(paneID, remotePrefix+machineID+"/")
			break
		}
	}
	if machineID == "" || rawID == "" {
		return PaneRead{}, errors.New("remote pane is unavailable; refresh the saved machine inventory")
	}
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	content, err := c.runCommand(readCtx, "--machine", machineID, "pane", "read", rawID,
		"--lines", strconv.Itoa(lines), "--format", format, "--source", source)
	if err != nil {
		return PaneRead{}, err
	}
	return PaneRead{Content: content}, nil
}
