package herdr

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestGetInventoryParsesAgentActivitySequence(t *testing.T) {
	socketPath, done := startUnaryTestSocket(t, "agent.list", map[string]any{
		"type": "agent_list",
		"agents": []any{
			map[string]any{
				"pane_id":          "pane-1",
				"agent":            "codex",
				"agent_status":     "idle",
				"state_change_seq": 794,
				"agent_session":    map[string]any{"value": "session-1", "kind": "id"},
			},
		},
	})
	client := NewClient(filepath.Join(t.TempDir(), "missing-herdr"), socketPath)

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
	if socketErr := <-done; socketErr != nil {
		t.Fatal(socketErr)
	}
}

func TestGetInventoryUsesJSONWithoutCLIFallback(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "herdr")
	script := "#!/bin/sh\nprintf '%s\\n' 'protocol_mismatch' >&2\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write Herdr script: %v", err)
	}
	socketPath, done := startUnaryTestSocket(t, "agent.list", map[string]any{
		"type":   "agent_list",
		"agents": []any{map[string]any{"pane_id": "pane-json", "agent": "codex", "agent_status": "idle"}},
	})
	client := NewClient(bin, socketPath)

	inventory, err := client.GetInventory(context.Background())
	if err != nil {
		t.Fatalf("GetInventory() error = %v", err)
	}
	if len(inventory.Panes) != 1 || inventory.Panes[0].ID != "pane-json" {
		t.Fatalf("GetInventory() = %#v, want JSON inventory", inventory)
	}
	if socketErr := <-done; socketErr != nil {
		t.Fatal(socketErr)
	}
}
