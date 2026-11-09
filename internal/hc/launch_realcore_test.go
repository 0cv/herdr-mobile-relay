package hc

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/hc/hctest"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func TestLaunchLayoutRealCore(t *testing.T) {
	var cwd string
	socket, _ := hctest.StartCore(t, func(root string, env []string) []string { cwd = root; return env })
	c := New(socket)
	pane, _, err := c.LaunchTerminal(t.Context(), []string{"/bin/sleep", "120"}, cwd, "requested", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	root := func(id, terminal string) map[string]any {
		return map[string]any{"id": id, "type": "block", "content": map[string]string{"terminal": terminal}}
	}
	value := map[string]any{"active": pane.WorkspaceID, "spaces": []any{map[string]any{
		"id": pane.WorkspaceID, "name": "Project", "folder": cwd, "active": pane.TabID,
		"tabs": []any{map[string]any{"id": "before", "name": "Before", "root": root("p0", "other")},
			map[string]any{"id": pane.TabID, "name": "", "focused": "p1", "root": root("p1", pane.ID)}},
	}}}
	if _, err := c.call(t.Context(), "doc.set", map[string]any{"name": "herdr-app", "version": 0, "value": value}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		tabs, err := c.TabList(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(tabs) == 2 && tabs[1].Label == "requested" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("tab not named: %+v", tabs)
		}
		time.Sleep(20 * time.Millisecond)
	}
	type launched struct {
		pane *herdr.Pane
		err  error
	}
	done := make(chan launched, 1)
	go func() {
		replacement, _, err := c.LaunchTerminal(t.Context(), []string{"/bin/sleep", "120"}, cwd, "replacement", pane.WorkspaceID, pane.TabID, false)
		done <- launched{replacement, err}
	}()
	var replacement herdr.Pane
	deadline = time.Now().Add(time.Second)
	for replacement.ID == "" {
		panes, err := c.PaneList(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range panes {
			if p.ID != pane.ID {
				replacement = p
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("replacement not launched")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := c.editTab(t.Context(), pane.TabID, func(tabs []map[string]json.RawMessage, index int) error {
		tabs[index]["root"], _ = json.Marshal(map[string]any{"id": "split", "type": "row", "children": []any{root("p1", pane.ID), root("p2", replacement.ID)}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	result := <-done
	if result.err != nil || result.pane.TabID != pane.TabID {
		t.Fatalf("replacement=%+v err=%v", result.pane, result.err)
	}
	if err := c.StopPane(t.Context(), pane.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.editTab(t.Context(), pane.TabID, func(tabs []map[string]json.RawMessage, index int) error {
		tabs[index]["root"], _ = json.Marshal(root("p2", replacement.ID))
		tabs[index]["focused"], _ = json.Marshal("p2")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	tabs, err := c.TabList(t.Context())
	if err != nil || len(tabs) != 2 || tabs[1].ID != pane.TabID || tabs[1].Label != "requested" {
		t.Fatalf("replacement layout=%+v err=%v", tabs, err)
	}
	if err := c.StopPane(t.Context(), replacement.ID); err != nil {
		t.Fatal(err)
	}
}

func TestLaunchRealCore(t *testing.T) {
	var cwd string
	socket, _ := hctest.StartCore(t, func(root string, env []string) []string { cwd = root; return env })
	c := New(socket)
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	pane, ready, err := c.LaunchTerminal(ctx, []string{"/bin/sleep", "120"}, cwd, "launch-fixture", "", "", true)
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
	replacement, ready, err := c.LaunchTerminal(t.Context(), []string{"/bin/sh", "-c", "printf '\\033]7501;state=idle:app=fixture\\007'; exec /bin/sleep 120"}, cwd, "replacement", pane.WorkspaceID, "", true)
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
