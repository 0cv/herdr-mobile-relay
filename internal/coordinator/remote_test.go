package coordinator

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func TestAppendRemoteInventoryPreservesLocalTopology(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "herdr")
	script := `#!/bin/sh
case "$*" in
 'machine list --json') echo '[{"id":"machine","label":"Remote Mac","enabled":true}]' ;;
 '--machine machine agent list') echo '{"result":{"agents":[{"pane_id":"w1:p1","workspace_id":"w1","tab_id":"w1:t1","terminal_id":"term1","agent":"claude","agent_status":"idle","cwd":"/remote/project"}]}}' ;;
 '--machine machine workspace list') echo '{"result":{"workspaces":[{"workspace_id":"w1","label":"Project"}]}}' ;;
 '--machine machine tab list') echo '{"result":{"tabs":[{"workspace_id":"w1","tab_id":"w1:t1","cwd":"/remote/project"}]}}' ;;
 *) exit 1 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	client := herdr.NewClient(bin, filepath.Join(dir, "missing.sock"))
	if err := client.RefreshRemoteInventory(t.Context()); err != nil {
		t.Fatal(err)
	}
	poller := NewPoller(client, testState(), time.Second, testLogger())
	local := []*AgentState{{PaneID: "w1:p1", WorkspaceID: "w1", Cwd: "/local/project"}}
	workspaces := make([]herdr.Workspace, 1, 5)
	workspaces[0] = herdr.Workspace{ID: "w1", Cwd: "/local/project"}
	for range 2 { // Both polling and event commits must retain the cached remote topology.
		agents, merged := poller.appendRemoteInventory(local, workspaces)
		if len(agents) != 2 || len(merged) != 2 {
			t.Fatal("remote topology missing")
		}
		if agents[0].PaneID != "w1:p1" || merged[0].Cwd != "/local/project" {
			t.Fatal("local topology modified")
		}
		remote := agents[1]
		if remote.PaneID != "remote/machine/w1:p1" || remote.Host != "Remote Mac" || !remote.ReadOnly || remote.MachineID != "machine" || remote.SessionID != "" {
			t.Fatalf("remote metadata=%#v", remote)
		}
		if merged[1].Cwd != "/remote/project" || merged[1].ID == merged[0].ID {
			t.Fatalf("remote workspace=%#v", merged[1])
		}
		merged[1].Label = "changed"
		if client.RemoteInventory().Workspaces[0].Label == "changed" {
			t.Fatal("shared remote snapshot mutated")
		}
	}
	if len(local) != 1 || len(workspaces) != 1 || workspaces[:cap(workspaces)][1].ID != "" {
		t.Fatal("caller backing storage modified")
	}
}
