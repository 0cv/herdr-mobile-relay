package hc

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/hc/hctest"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func TestRealCore(t *testing.T) {
	var root string
	socket, _ := hctest.StartCore(t, func(scratch string, env []string) []string {
		root = scratch
		return env
	})
	client := New(socket)
	var started []string
	t.Cleanup(func() {
		for _, addr := range started {
			for _, method := range []string{"terminal.kill", "terminal.rm"} {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_, err := client.call(ctx, method, map[string]any{"terminal": addr})
				cancel()
				if err != nil {
					t.Errorf("cleanup %s %s: %v", method, addr, err)
				}
			}
		}
	})
	wait := func(t *testing.T, ready func() bool) {
		t.Helper()
		deadline := time.NewTimer(10 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for !ready() {
			select {
			case <-deadline.C:
				t.Fatal("condition not satisfied within 10s")
			case <-tick.C:
			}
		}
	}
	wait(t, func() bool { _, err := os.Stat(socket); return err == nil })
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	start := func(argv []string) string {
		raw, err := client.call(ctx, "terminal.run", map[string]any{"argv": argv, "cwd": root, "no_parent": true, "labels": map[string]string{"herdr.space": "relay-space", "herdr.tab": "relay-tab"}})
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Address string `json:"address"`
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Fatal(err)
		}
		if result.Address == "" {
			t.Fatalf("terminal.run has no address: %s", raw)
		}
		started = append(started, result.Address)
		return result.Address
	}
	addr := start([]string{"/bin/sh"})
	t.Run("labels", func(t *testing.T) {
		panes, err := client.PaneList(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, pane := range panes {
			if pane.ID == addr {
				if pane.WorkspaceID != "relay-space" || pane.TabID != "relay-tab" {
					t.Fatalf("labels: %+v", pane)
				}
				return
			}
		}
		t.Fatal("started shell missing from PaneList")
	})
	t.Run("idle-shell", func(t *testing.T) {
		wait(t, func() bool {
			info, err := client.PaneProcessInfoFresh(ctx, addr)
			if err != nil {
				t.Fatal(err)
			}
			return info.ShellPID > 0 && info.ShellPID == info.ForegroundProcessGroupID
		})
	})
	snapshot, stream, err := client.Events(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	t.Run("snapshot", func(t *testing.T) {
		for _, pane := range snapshot.Panes {
			if pane.ID == addr {
				return
			}
		}
		t.Fatal("shell missing from Events snapshot")
	})
	t.Run("prompt-and-visible-read", func(t *testing.T) {
		if err := client.Prompt(ctx, addr, "echo hc-relay-mark"); err != nil {
			t.Fatal(err)
		}
		wait(t, func() bool {
			read, err := client.ReadPaneVisible(ctx, addr, 0, "text")
			if err != nil {
				t.Fatal(err)
			}
			return strings.Contains(string(read.Content), "hc-relay-mark") && !read.Truncated
		})
	})
	cat := start([]string{"/bin/sh", "-c", "trap '' INT; exec cat"})
	for _, key := range []string{"Enter", "Escape", "Tab", "Up", "Down", "Left", "Right", "Shift+Tab", "ctrl+c", "f5", "A"} {
		t.Run("key/"+key, func(t *testing.T) {
			if err := client.SendKeys(ctx, cat, []string{key}); err != nil {
				t.Fatalf("client/core key mismatch for %q: %v", key, err)
			}
		})
	}
	for _, key := range []string{"/", "A", "1", " "} {
		t.Run("literal/"+key, func(t *testing.T) {
			if err := client.SendLiteralKey(ctx, cat, key); err != nil {
				t.Fatalf("client/core literal key mismatch for %q: %v", key, err)
			}
		})
	}
	t.Run("unknown-key-refused", func(t *testing.T) {
		err := client.SendKeys(ctx, cat, []string{"not-a-real-key"})
		if !herdr.IsRefused(err) || errors.Is(err, herdr.ErrDispatchedUnknown) {
			t.Fatalf("unknown key classification: %v", err)
		}
	})
	t.Run("status-event-and-inventory", func(t *testing.T) {
		if err := client.Prompt(ctx, addr, `printf '\033]7501;state=blocked:kind=permission:app=relayit\033\\'`); err != nil {
			t.Fatal(err)
		}
		eventCtx, stop := context.WithTimeout(ctx, 10*time.Second)
		defer stop()
		for {
			snapshot, err := stream.Next(eventCtx)
			if err != nil {
				t.Fatalf("status stream: %v", err)
			}
			found := false
			for _, pane := range snapshot.Panes {
				if pane.ID == addr && pane.Agent == "relayit" && pane.Status == "blocked" {
					found = true
				}
			}
			if found {
				break
			}
		}
		inventory, err := client.GetInventory(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, pane := range inventory.Panes {
			if pane.ID == addr && pane.Agent == "relayit" && pane.Status == "blocked" {
				return
			}
		}
		t.Fatalf("reported status missing from inventory: %+v", inventory)
	})
	t.Run("stop", func(t *testing.T) {
		if err := client.StopPane(ctx, addr); err != nil {
			t.Fatal(err)
		}
		wait(t, func() bool {
			panes, err := client.PaneList(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, pane := range panes {
				if pane.ID == addr {
					return false
				}
			}
			return true
		})
	})
}
