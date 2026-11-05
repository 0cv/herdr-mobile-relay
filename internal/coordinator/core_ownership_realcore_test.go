package coordinator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/hc"
	"github.com/0cv/herdr-mobile-relay/internal/hc/hctest"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/profiles"
)

func TestCoreOwnershipRealCore(t *testing.T) {
	var root string
	socket, env := hctest.StartCore(t, func(scratch string, env []string) []string { root = scratch; return env })
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		if key == "HOME" || strings.HasPrefix(key, "XDG_") {
			t.Setenv(key, value)
		}
	}
	home := filepath.Join(root, "home")
	cwd := filepath.Join(home, "project")
	config := filepath.Join(root, "config")
	bin := filepath.Join(root, "bin")
	for _, dir := range []string{cwd, bin, filepath.Join(config, "herdr")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	gate := filepath.Join(root, "report")
	script := "#!/bin/sh\nwhile [ ! -f '" + gate + "' ]; do /bin/sleep 0.02; done\nprintf '\033]7501;state=idle:app=fixture\007'\nexec /bin/sleep 120\n"
	if err := os.WriteFile(filepath.Join(bin, "fixture"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(config, "herdr", "agent-profiles.ini"), []byte("[profiles]\nfixture = Fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	resolver := profiles.NewResolver(config, nil)
	ownership := filepath.Join(root, "ownership.json")
	if err := resolver.SetOwnershipPath(ownership); err != nil {
		t.Fatal(err)
	}
	client := hc.New(socket)
	state := NewState(testLogger())
	d := NewDispatcher(client, state, nil, testLogger())
	d.SetProfiles(resolver)
	defer d.Close(context.Background())
	poller := NewPoller(client, state, time.Hour, testLogger())
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	result := d.Handle(ctx, map[string]any{"action": "agent_start", "request_id": "launch", "profile_id": "fixture", "name": "fixture", "cwd": cwd})
	if !result.OK {
		t.Fatalf("launch: %+v", result)
	}
	old := result.PaneID
	pane, err := client.PaneGet(ctx, old)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		poller.poll(ctx)
	}
	inventory, err := client.GetInventory(ctx)
	if err != nil || len(inventory.Panes) != 0 {
		t.Fatalf("unreported inventory: %+v %v", inventory, err)
	}
	if resolver.ResolvePane(old, "") != "fixture" {
		t.Fatal("unreported launch lost ownership")
	}
	first, stream, err := client.Events(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stream.Close()
	second, stream, err := client.Events(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stream.Close()
	if first.Panes[0].TerminalID != second.Panes[0].TerminalID {
		t.Fatal("reconnect changed identity")
	}
	reloaded := profiles.NewResolver(config, nil)
	if err := reloaded.SetOwnershipPath(ownership); err != nil {
		t.Fatal(err)
	}
	client = hc.New(socket)
	d.herdr = client
	d.SetProfiles(reloaded)
	poller = NewPoller(client, state, time.Hour, testLogger())
	poller.poll(ctx)
	if reloaded.ResolvePane(old, "") != "fixture" {
		t.Fatal("relay restart lost ownership")
	}
	if err := os.WriteFile(gate, nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitAgent := func(id string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			poller.poll(ctx)
			if _, ok := state.Agent(id); ok {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("agent %s not detected", id)
	}
	waitAgent(old)
	observed, err := client.PaneGet(ctx, old)
	if err != nil || pane.TabID != observed.TabID || pane.WorkspaceID != observed.WorkspaceID {
		t.Fatalf("labels changed: %+v %+v %v", pane, observed, err)
	}
	for _, action := range []string{"agent_clear", "agent_restart"} {
		result = d.Handle(ctx, map[string]any{"action": action, "request_id": action, "pane_id": old})
		if !result.OK {
			t.Fatalf("%s: %+v", action, result)
		}
		data := result.Data.(map[string]any)
		replacement := data["pane_id"].(string)
		if replacement == old {
			t.Fatal("terminal not replaced")
		}
		if _, err := client.PaneGet(ctx, old); !herdr.IsPaneNotFound(err) {
			t.Fatalf("old terminal still live: %v", err)
		}
		waitAgent(replacement)
		if reloaded.ResolvePane(replacement, "") != "fixture" {
			t.Fatal("replacement ownership missing")
		}
		old = replacement
	}
	if err := client.StopPane(ctx, old); err != nil {
		t.Fatal(err)
	}
}
