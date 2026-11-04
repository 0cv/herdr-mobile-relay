package hc

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/hc/hctest"
)

func TestLaunchRealCore(t *testing.T) {
	var cwd string
	socket, _ := hctest.StartCore(t, func(root string, env []string) []string { cwd = root; return env })
	c := New(socket)
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	pane, ready, err := c.LaunchTerminal(ctx, []string{"/bin/sleep", "120"}, cwd, "launch-fixture", "", true)
	if err != nil || pane == nil || ready {
		t.Fatalf("launch = %v %v %v", pane, ready, err)
	}
	raw, err := c.terminalCall(t.Context(), "terminal.get", pane.ID, map[string]any{})
	var launched terminal
	if err != nil || json.Unmarshal(raw, &launched) != nil || launched.Labels["herdr.seen"] != "0" || launched.Labels["herdr.space"] != pane.WorkspaceID || launched.Labels["herdr.tab"] != pane.TabID {
		t.Fatalf("terminal=%s err=%v", raw, err)
	}
	panes, err := c.PaneList(t.Context())
	if err != nil || len(panes) != 1 || panes[0].ID != pane.ID || panes[0].Name != "launch-fixture" {
		t.Fatalf("panes=%+v err=%v", panes, err)
	}
	gotCwd, _ := filepath.EvalSymlinks(panes[0].Cwd)
	wantCwd, _ := filepath.EvalSymlinks(cwd)
	if gotCwd != wantCwd {
		t.Fatalf("cwd=%s want=%s", gotCwd, wantCwd)
	}
	spaces, err := c.WorkspaceList(t.Context())
	if err != nil || len(spaces) != 1 || spaces[0].ID != pane.WorkspaceID {
		t.Fatalf("spaces=%+v err=%v", spaces, err)
	}
	tabs, err := c.TabList(t.Context())
	if err != nil || len(tabs) != 1 || tabs[0].ID != pane.TabID {
		t.Fatalf("tabs=%+v err=%v", tabs, err)
	}
	replacement, ready, err := c.LaunchTerminal(t.Context(), []string{"/bin/sh", "-c", "printf '\\033]7501;state=idle:app=fixture\\007'; exec /bin/sleep 120"}, cwd, "replacement", pane.WorkspaceID, true)
	if err != nil || replacement.ID == pane.ID || !ready {
		t.Fatalf("replacement=%+v err=%v", replacement, err)
	}
	if err := c.StopPane(t.Context(), pane.ID); err != nil {
		t.Fatal(err)
	}
	panes, err = c.PaneList(t.Context())
	if err != nil || len(panes) != 1 || panes[0].ID != replacement.ID {
		t.Fatalf("after kill=%+v err=%v", panes, err)
	}
	if err := c.StopPane(t.Context(), replacement.ID); err != nil {
		t.Fatal(err)
	}
}
