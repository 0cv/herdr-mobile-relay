package herdr

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestGetInventoryParsesAgentActivitySequence(t *testing.T) {
	bin := writeResultScript(t, `{"result":{"agents":[{"pane_id":"pane-1","agent":"codex","agent_status":"idle","state_change_seq":794,"agent_session":{"value":"session-1","kind":"id"}}]}}`)
	client := NewClient(bin, filepath.Join(t.TempDir(), "herdr.sock"))

	inventory, err := client.GetInventory(context.Background())
	if err != nil {
		t.Fatalf("GetInventory() error = %v", err)
	}
	if len(inventory.Panes) != 1 {
		t.Fatalf("GetInventory() panes = %d, want 1", len(inventory.Panes))
	}
	pane := inventory.Panes[0]
	if pane.StateChangeSeq != 794 || pane.Session != "session-1" {
		t.Fatalf("GetInventory() pane = %#v", pane)
	}
}

func TestGetInventoryFallsBackToPaneList(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "herdr")
	script := `#!/bin/sh
if [ "$1 $2" = "agent list" ]; then
  echo "unsupported command" >&2
  exit 1
fi
printf '%s\n' '{"result":{"panes":[{"pane_id":"pane-legacy","agent":"codex","agent_status":"idle"}]}}'
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write Herdr script: %v", err)
	}
	client := NewClient(bin, filepath.Join(t.TempDir(), "herdr.sock"))

	inventory, err := client.GetInventory(context.Background())
	if err != nil {
		t.Fatalf("GetInventory() error = %v", err)
	}
	if len(inventory.Panes) != 1 || inventory.Panes[0].ID != "pane-legacy" {
		t.Fatalf("GetInventory() = %#v, want pane-list fallback", inventory)
	}
}
