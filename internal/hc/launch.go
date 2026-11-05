package hc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

const adoptionTimeout = 5 * time.Second

func (c *Client) nameAdoptedTab(tabID, name string) {
	_ = pollAdoption(context.Background(), time.Minute, func(ctx context.Context) error {
		return c.TabRename(ctx, tabID, name)
	})
}

func pollAdoption(ctx context.Context, timeout time.Duration, attempt func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := attempt(ctx); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

type adoptionNode struct {
	Content struct {
		Terminal string `json:"terminal"`
	} `json:"content"`
	Children []adoptionNode `json:"children"`
}

func (n adoptionNode) contains(terminalID string) bool {
	if n.Content.Terminal == terminalID {
		return true
	}
	for _, child := range n.Children {
		if child.contains(terminalID) {
			return true
		}
	}
	return false
}

func (c *Client) waitForAdoption(ctx context.Context, tabID, terminalID string) error {
	return pollAdoption(ctx, adoptionTimeout, func(ctx context.Context) error {
		raw, err := c.call(ctx, "doc.get", map[string]any{"name": "herdr-app"})
		if err != nil {
			return err
		}
		var doc struct {
			Value struct {
				Spaces []struct {
					Tabs []struct {
						ID   string       `json:"id"`
						Root adoptionNode `json:"root"`
					} `json:"tabs"`
				} `json:"spaces"`
			} `json:"value"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			return err
		}
		for _, space := range doc.Value.Spaces {
			for _, tab := range space.Tabs {
				if tab.ID == tabID && tab.Root.contains(terminalID) {
					return nil
				}
			}
		}
		return invalidLayout()
	})
}

func launchID(prefix string) string {
	var id [16]byte
	rand.Read(id[:])
	return prefix + hex.EncodeToString(id[:])
}

func (c *Client) LaunchTerminal(ctx context.Context, argv []string, cwd, name, workspaceID, tabID string, waitReady bool) (*herdr.Pane, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if len(argv) == 0 || !filepath.IsAbs(argv[0]) || !filepath.IsAbs(cwd) {
		return nil, false, errors.New("launch requires an absolute executable and cwd")
	}
	if workspaceID == "" || workspaceID == "core" || strings.Contains(workspaceID, "/") {
		workspaceID = launchID("sp")
	}
	reuseTab := localLayoutID(tabID)
	if !reuseTab {
		tabID = launchID("tb")
	}
	raw, err := c.call(ctx, "terminal.run", map[string]any{
		"argv": argv, "cwd": cwd, "name": name,
		"labels": map[string]string{"herdr.space": workspaceID, "herdr.tab": tabID, "herdr.seen": "0"},
		"keep":   true, "no_parent": true, "wait": false,
	})
	if err != nil {
		return nil, false, err
	}
	var launched terminal
	if err := json.Unmarshal(raw, &launched); err != nil {
		return nil, false, errors.Join(herdr.ErrDispatchedUnknown, err)
	}
	if !launched.listed() {
		return nil, false, errors.Join(herdr.ErrDispatchedUnknown, errors.New("launched terminal is not live"))
	}
	pane := c.pane(launched)
	if reuseTab {
		_ = c.waitForAdoption(ctx, tabID, pane.ID)
	} else if name != "" {
		go c.nameAdoptedTab(tabID, name)
	}
	if !waitReady {
		return &pane, false, nil
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		raw, err := c.terminalCall(ctx, "terminal.get", pane.ID, map[string]any{})
		if err != nil {
			return &pane, false, nil
		}
		var current terminal
		if json.Unmarshal(raw, &current) != nil || !current.listed() || c.pane(current).TerminalID != pane.TerminalID {
			return &pane, false, nil
		}
		if current.Reported && current.Status != nil && current.Status.State == "idle" {
			return &pane, true, nil
		}
		select {
		case <-ctx.Done():
			return &pane, false, nil
		case <-ticker.C:
		}
	}
}
