package herdr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func remoteFixture(t *testing.T) (*Client, string) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
printf '%s\n' "$*" >> '` + dir + `/calls'
if [ "$1" = machine ]; then cat '` + dir + `/machines'; exit; fi
if [ "$1" != --machine ]; then echo local-fallback >&2; exit 9; fi
if [ "$2" = offline ]; then exit 1; fi
# Use a single executable for the deadline propagation test. A shell waiting
# on sleep can leave an orphan zombie on Linux, exercising the separate
# process-group cleanup grace period instead of this request's cancellation.
if [ "$2" = slow ]; then exec sleep 30; fi
case "$3 $4" in
 'agent list') printf '%s' '{"result":{"agents":[{"pane_id":"w1:p1","terminal_id":"term1","tab_id":"w1:t1","workspace_id":"w1","agent":"claude","cwd":"/remote/project","agent_status":"idle","agent_session":{"value":"remote-session"}}]}}' ;;
 'workspace list') printf '%s' '{"result":{"workspaces":[{"workspace_id":"w1","label":"Project","active_tab_id":"w1:t1"}]}}' ;;
 'tab list') printf '%s' '{"result":{"tabs":[{"tab_id":"w1:t1","workspace_id":"w1","cwd":"/remote/project"}]}}' ;;
 'pane read') printf '%s' 'remote terminal output' ;;
 *) exit 2 ;;
esac
`
	bin := filepath.Join(dir, "herdr")
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return NewClient(bin, filepath.Join(dir, "missing.sock")), dir
}

func setMachines(t *testing.T, dir, machines string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "machines"), []byte(machines), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteInventoryIsolationAndReads(t *testing.T) {
	client, dir := remoteFixture(t)
	setMachines(t, dir, `[{"id":"one","label":"Mac One","enabled":true},{"id":"two","label":"Mac Two","enabled":true},{"id":"disabled","enabled":false},{"id":"offline","enabled":true}]`)
	if err := client.RefreshRemoteInventory(t.Context()); err == nil {
		t.Fatal("expected isolated offline error")
	}
	inv := client.RemoteInventory()
	if len(inv.Panes) != 2 || len(inv.Workspaces) != 2 || len(inv.Tabs) != 2 {
		t.Fatalf("snapshot = %#v", inv)
	}
	for i, machine := range []string{"one", "two"} {
		p := inv.Panes[i]
		if p.ID != remoteID(machine, "w1:p1") || p.TerminalID != remoteID(machine, "term1") || p.TabID != inv.Tabs[i].ID || p.WorkspaceID != inv.Workspaces[i].ID || p.Session != "" {
			t.Fatalf("wrong namespace: %#v", p)
		}
		if !inv.Workspaces[i].ReadOnly || inv.Workspaces[i].MachineID != machine {
			t.Fatalf("workspace missing remote metadata: %#v", inv.Workspaces[i])
		}
		read, err := client.ReadPane(t.Context(), p.ID, 30, "text")
		if err != nil || string(read.Content) != "remote terminal output" {
			t.Fatalf("read=%q err=%v", read.Content, err)
		}
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if strings.Contains(string(calls), "--machine disabled") || !strings.Contains(string(calls), "--machine one pane read w1:p1") {
		t.Fatalf("wrong routing: %s", calls)
	}
	before := string(calls)
	for _, id := range []string{"remote/offline/w1:p1", "remote/unknown/w1:p1", "remote/", "remote/one/"} {
		if _, err := client.ReadPane(t.Context(), id, 30, "text"); err == nil {
			t.Fatalf("unavailable read accepted: %s", id)
		}
	}
	calls, _ = os.ReadFile(filepath.Join(dir, "calls"))
	if string(calls) != before {
		t.Fatal("unavailable remote invoked a command")
	}
	setMachines(t, dir, `[]`)
	if err := client.RefreshRemoteInventory(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(client.RemoteInventory().Panes) != 0 {
		t.Fatal("removed machine retained")
	}
	if _, err := client.ReadPane(t.Context(), "remote/one/w1:p1", 30, "text"); err == nil {
		t.Fatal("removed machine still readable")
	}
}

func TestRemoteDiscoveryMalformedAndCancellation(t *testing.T) {
	client, dir := remoteFixture(t)
	setMachines(t, dir, `[{"id":"one","enabled":true}]`)
	if err := client.RefreshRemoteInventory(t.Context()); err != nil {
		t.Fatal(err)
	}
	setMachines(t, dir, `not JSON`)
	if err := client.RefreshRemoteInventory(t.Context()); err == nil {
		t.Fatal("malformed discovery accepted")
	}
	if len(client.RemoteInventory().Panes) != 0 {
		t.Fatal("stale inventory after malformed discovery")
	}
	setMachines(t, dir, `[{"id":"slow","enabled":true}]`)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := client.RefreshRemoteInventory(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline cancellation, got %v", err)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("remote cancellation did not stop the CLI")
	}
}

func TestRemoteInventoryConcurrentSnapshot(t *testing.T) {
	client, dir := remoteFixture(t)
	setMachines(t, dir, `[{"id":"one","enabled":true}]`)
	done := make(chan error, 1)
	go func() { done <- client.RefreshRemoteInventory(t.Context()) }()
	for range 1000 {
		_ = client.RemoteInventory()
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// Opt-in smoke test: queries saved machine inventory and reads terminal output,
// but never sends prompts, keys, or any remote mutation.
func TestSavedRemoteMachineSmoke(t *testing.T) {
	machine := os.Getenv("HERDR_REMOTE_SMOKE_MACHINE")
	if machine == "" {
		t.Skip("set HERDR_REMOTE_SMOKE_MACHINE to opt into read-only live verification")
	}
	bin := os.Getenv("HERDR_BIN")
	if bin == "" {
		bin = "herdr"
	}
	client := NewClient(bin, os.Getenv("HERDR_SOCKET_PATH"))
	if err := client.RefreshRemoteInventory(t.Context()); err != nil {
		t.Fatal(err)
	}
	inventory := client.RemoteInventory()
	found := false
	for _, pane := range inventory.Panes {
		if pane.MachineID != machine && pane.MachineLabel != machine {
			continue
		}
		read, err := client.ReadPane(t.Context(), pane.ID, 10, "text")
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("machine=%s agent=%s pane=%s terminal_bytes=%d", pane.MachineLabel, pane.Agent, pane.ID, len(read.Content))
		found = true
	}
	if !found {
		t.Fatal("requested saved machine has no discoverable agents")
	}
	t.Logf("remote snapshot: %d agents, %d workspaces, %d tabs", len(inventory.Panes), len(inventory.Workspaces), len(inventory.Tabs))
}
