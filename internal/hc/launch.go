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

func launchID(prefix string) string {
	var id [16]byte
	rand.Read(id[:])
	return prefix + hex.EncodeToString(id[:])
}

func (c *Client) LaunchTerminal(ctx context.Context, argv []string, cwd, name, workspaceID string, waitReady bool) (*herdr.Pane, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if len(argv) == 0 || !filepath.IsAbs(argv[0]) || !filepath.IsAbs(cwd) {
		return nil, false, errors.New("launch requires an absolute executable and cwd")
	}
	if workspaceID == "" || workspaceID == "core" || strings.Contains(workspaceID, "/") {
		workspaceID = launchID("sp")
	}
	raw, err := c.call(ctx, "terminal.run", map[string]any{
		"argv": argv, "cwd": cwd, "name": name,
		"labels": map[string]string{"herdr.space": workspaceID, "herdr.tab": launchID("tb"), "herdr.seen": "0"},
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
