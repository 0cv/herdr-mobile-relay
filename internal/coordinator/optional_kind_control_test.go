package coordinator

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func TestAuthoritativeOptionalKindInventory(t *testing.T) {
	for _, mode := range []string{"poll", "event"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var reply atomic.Value
			reply.Store(json.RawMessage(`{"type":"agent_list","agents":[{"pane_id":"pane","terminal_id":"terminal","workspace_id":"workspace","tab_id":"tab","name":"Named agent","agent_status":"unknown","cwd":"/home/example/Development"}]}`))
			var failed atomic.Bool
			socket := startPollerTestSocket(t, func(method string) (any, string) {
				switch method {
				case "agent.list":
					if failed.Load() {
						return nil, "internal_error"
					}
					return reply.Load().(json.RawMessage), ""
				case "pane.list":
					return json.RawMessage(`{"type":"pane_list","panes":[{"pane_id":"shell","terminal_id":"shell-terminal","name":"Shell","cwd":"/home/example/Development"}]}`), ""
				case "workspace.list":
					return json.RawMessage(`{"type":"workspace_list","workspaces":[]}`), ""
				case "tab.list":
					return json.RawMessage(`{"type":"tab_list","tabs":[]}`), ""
				default:
					t.Errorf("unexpected method %s", method)
					return nil, "unsupported"
				}
			})
			client := herdr.NewClient("unused", socket)
			t.Cleanup(func() { _ = client.Close() })
			inventory, err := client.GetInventory(ctx)
			if err != nil || len(inventory.Panes) != 1 || inventory.Panes[0].Agent != "" || inventory.Panes[0].Session != "" {
				t.Fatalf("optional fields rejected by GetInventory: %+v %v", inventory, err)
			}
			state := testState()
			poller := NewPoller(client, state, time.Second, testLogger())
			publications := 0
			poller.SetOnInventoryChange(func() error { publications++; return nil })
			refresh := func() {
				if mode == "poll" {
					poller.poll(ctx)
					return
				}
				poller.commitEventTopology(ctx, herdr.TopologySnapshot{Panes: []herdr.Pane{{ID: "shell", Agent: "hermes"}}}, state.RevisionCounter())
			}
			assertKind := func(kind string) {
				t.Helper()
				refresh()
				row, ok := state.Agent("pane")
				if !ok || row.Agent != kind || row.TerminalID != "terminal" || row.Name != "Named agent" || row.Cwd != "/home/example/Development" || row.Project != "Development" || row.Session != "" {
					t.Fatalf("authoritative agent row was dropped or guessed: %+v present=%v", row, ok)
				}
				if _, ok := state.Agent("shell"); ok {
					t.Fatal("pane.list or cached topology promoted a shell")
				}
				if publications == 0 || state.InventoryStatus()["state"] != "ready" {
					t.Fatal("inventory was not published ready")
				}
			}
			assertKind("")
			for _, kind := range []string{"hermes", "hermes-agent", "custom-agent", ""} {
				encoded, _ := json.Marshal(kind)
				reply.Store(json.RawMessage(`{"type":"agent_list","agents":[{"pane_id":"pane","terminal_id":"terminal","workspace_id":"workspace","tab_id":"tab","name":"Named agent","agent_status":"unknown","cwd":"/home/example/Development","agent":` + string(encoded) + `}]}`))
				assertKind(kind)
			}
			failed.Store(true)
			refresh()
			if _, ok := state.Agent("pane"); !ok || state.InventoryStatus()["state"] != "error" || state.InventoryStatus()["stale"] != true {
				t.Fatal("failed inventory removed the row or claimed fresh state")
			}
			failed.Store(false)
			reply.Store(json.RawMessage(`{"type":"agent_list","agents":[]}`))
			refresh()
			if _, ok := state.Agent("pane"); ok {
				t.Fatal("removed authoritative row survived")
			}
		})
	}
}
