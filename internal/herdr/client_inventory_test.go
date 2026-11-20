package herdr

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestGetInventorySocketAvailability(t *testing.T) {
	tests := []struct {
		name           string
		setup          func(string) error
		wantNotRunning bool
		live           bool
	}{
		{name: "missing", wantNotRunning: true},
		{name: "stale socket", wantNotRunning: true, setup: func(path string) error {
			listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
			if err != nil {
				return err
			}
			listener.SetUnlinkOnClose(false)
			return listener.Close()
		}},
		{name: "regular file", setup: func(path string) error { return os.WriteFile(path, nil, 0o600) }},
		{name: "directory", setup: func(path string) error { return os.Mkdir(path, 0o700) }},
		{name: "dangling symlink", setup: func(path string) error { return os.Symlink(path+".missing", path) }},
		{name: "live listener", live: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var path string
			var done <-chan error
			if tt.live {
				path, done = startUnaryTestSocket(t, "agent.list", map[string]any{
					"type": "agent_list", "agents": []Pane{},
				})
			} else {
				path = filepath.Join(testSocketDir(t), "herdr.sock")
				if tt.setup != nil {
					if err := tt.setup(path); err != nil {
						t.Fatal(err)
					}
				}
			}

			client := NewClient("/binary/must-not-run", path)
			_, err := client.GetInventory(context.Background())
			if tt.live {
				if err != nil {
					t.Fatalf("GetInventory() error = %v", err)
				}
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				return
			}
			if !errors.Is(err, ErrNotStarted) || errors.Is(err, ErrServerNotRunning) != tt.wantNotRunning {
				t.Fatalf("GetInventory() error = %v, want ErrNotStarted and ErrServerNotRunning=%t", err, tt.wantNotRunning)
			}
		})
	}
}

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
	if pane.StateChangeSeq != 794 || pane.Session.Value != "session-1" {
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
