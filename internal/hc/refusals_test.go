package hc

import (
	"errors"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"net"
	"testing"
)

func TestUnsupportedMethods(t *testing.T) {
	c := New(fakeCore(t, 1, func(conn net.Conn, req fakeRequest) { t.Errorf("unexpected request: %+v", req) }))
	for name, run := range map[string]func() error{
		"SendInput":          func() error { return c.SendInput(t.Context(), "term", herdr.PaneInput{}) },
		"SendText":           func() error { return c.SendText(t.Context(), "term", "term") },
		"PaneRun":            func() error { return c.PaneRun(t.Context(), "term", []string{"term"}) },
		"StartAgent":         func() error { _, err := c.StartAgent(t.Context(), "term", "term", "term", 0); return err },
		"RenameAgent":        func() error { return c.RenameAgent(t.Context(), "term", "term") },
		"AgentGet":           func() error { _, err := c.AgentGet(t.Context(), "term"); return err },
		"TabCreate":          func() error { _, err := c.TabCreate(t.Context(), "term", "term", "term"); return err },
		"WorkspaceCreate":    func() error { _, err := c.WorkspaceCreate(t.Context(), "term", "term"); return err },
		"WorkspaceRename":    func() error { return c.WorkspaceRename(t.Context(), "term", "term") },
		"WorkspaceMove":      func() error { return c.WorkspaceMove(t.Context(), "term", 0) },
		"WorkspaceMoveBlock": func() error { return c.WorkspaceMoveBlock(t.Context(), []string{"term"}, "term") },
		"WorkspaceClose":     func() error { return c.WorkspaceClose(t.Context(), "term", false) },
		"WorktreeList":       func() error { _, err := c.WorktreeList(t.Context(), "term"); return err },
		"WorktreeCreate":     func() error { _, err := c.WorktreeCreate(t.Context(), "term", "term", "term", "term"); return err },
		"WorktreeOpen":       func() error { _, err := c.WorktreeOpen(t.Context(), "term", "term", "term", "term"); return err },
		"WorktreeRemove":     func() error { _, err := c.WorktreeRemove(t.Context(), "term", false); return err },
	} {
		t.Run(name, func(t *testing.T) {
			err := run()
			var cli *herdr.CLIError
			if !herdr.IsRefused(err) || !errors.As(err, &cli) || cli.Code != "unsupported_method" || cli.Message == "" {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
