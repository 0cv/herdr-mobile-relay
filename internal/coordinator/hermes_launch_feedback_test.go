package coordinator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/profiles"
)

func TestHermesLaunchReceiptFollowsStartNotTabCreation(t *testing.T) {
	for _, outcome := range []string{"completed", "refused", "malformed", "cancelled", "timeout"} {
		t.Run(outcome, func(t *testing.T) {
			dir := t.TempDir()
			home := filepath.Join(dir, "home")
			cwd := filepath.Join(home, "Development")
			if err := os.MkdirAll(cwd, 0700); err != nil {
				t.Fatal(err)
			}
			writeScript(t, dir, "hermes", "#!/bin/sh\nexit 0\n")
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			record := filepath.Join(dir, "calls")
			entered, release := filepath.Join(dir, "entered"), filepath.Join(dir, "release")
			response := "printf '%s' '{\"result\":{\"agent\":{\"pane_id\":\"pane-new\"}}}'"
			if outcome == "refused" {
				response = "printf '%s' '{\"error\":{\"code\":\"agent_pane_busy\",\"message\":\"not an available shell\"}}' >&2; exit 1"
			}
			if outcome == "malformed" {
				response = "printf '%s' '{\"result\":null}'"
			}
			bin := writeScript(t, dir, "herdr", "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \""+record+"\"\ncase \"$1 $2\" in\n"+
				" 'workspace list') printf '%s' '{\"result\":{\"workspaces\":[{\"workspace_id\":\"workspace\"}]}}' ;;\n"+
				" 'tab create') printf '%s' '{\"result\":{\"root_pane\":{\"pane_id\":\"pane-new\"},\"tab\":{\"tab_id\":\"tab-new\",\"workspace_id\":\"workspace\"}}}' ;;\n"+
				" 'agent start') touch \""+entered+"\"; while [ ! -f \""+release+"\" ]; do sleep 0.01; done; "+response+" ;;\n"+
				" *) exit 2 ;;\nesac\n")
			socket := filepath.Join(testSocketDir(t), "herdr.sock")
			startInventorySocket(t, socket, []any{map[string]any{"workspace_id": "workspace"}}, herdr.Pane{ID: "pane-new", TerminalID: "terminal-new", TabID: "tab-new", WorkspaceID: "workspace"})
			client := herdr.NewClient(bin, socket)
			d := NewDispatcher(client, NewState(testLogger()), nil, testLogger())
			d.SetProfiles(profiles.NewResolver(filepath.Join(dir, "config"), nil))
			d.lifecycle.home = home
			stopAfterRefusals(d.lifecycle, 1)
			t.Cleanup(func() {
				_ = os.WriteFile(release, nil, 0600)
				_ = d.Close(context.Background())
			})
			budget := 8 * time.Second
			if outcome == "timeout" {
				budget = agentStartResponseReserve + 2*time.Second
			}
			ctx, cancel := context.WithTimeout(t.Context(), budget)
			defer cancel()
			message := map[string]any{"type": "agent_start", "request_id": "hermes-launch", "profile_id": "hermes", "workspace_id": "workspace", "name": "development-hermes", "cwd": cwd}
			pending := make(chan *CommandResult, 1)
			go func() { pending <- d.Handle(ctx, message) }()
			for {
				if _, err := os.Stat(entered); err == nil {
					break
				}
				select {
				case result := <-pending:
					t.Fatalf("start returned before fake Herdr started: %+v", result)
				case <-ctx.Done():
					t.Fatal("fake Herdr did not receive agent start")
				case <-time.After(time.Millisecond):
				}
			}
			select {
			case result := <-pending:
				t.Fatalf("tab creation was incorrectly treated as start completion: %+v", result)
			default:
			}
			if outcome == "cancelled" {
				cancel()
			} else if outcome != "timeout" {
				if err := os.WriteFile(release, nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			var result *CommandResult
			select {
			case result = <-pending:
			case <-time.After(5 * time.Second):
				t.Fatal("start did not settle after Herdr result or cancellation")
			}
			if result.OK != (outcome == "completed") {
				t.Fatalf("result = %+v", result)
			}
			if outcome != "cancelled" && result.PaneID != "pane-new" {
				t.Fatalf("created pane was lost: %+v", result)
			}
			if (outcome == "malformed" || outcome == "timeout") && result.Phase != "dispatched_unknown" {
				t.Fatalf("unconfirmed start was not uncertain: %+v", result)
			}
			if outcome == "completed" {
				replay := d.Handle(ctx, message)
				if !replay.OK || !replay.replayed {
					t.Fatalf("duplicate did not replay the receipt: %+v", replay)
				}
			}
			calls, err := os.ReadFile(record)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(calls), "agent start development-hermes --kind hermes --pane pane-new --timeout ") != 1 || strings.Count(string(calls), "tab create ") != 1 {
				t.Fatalf("unexpected launch calls:\n%s", calls)
			}
			if strings.Contains(string(calls), "--focus") || strings.Contains(string(calls), "pane close") || strings.Contains(string(calls), "agent get") || strings.Contains(string(calls), "pane run") {
				t.Fatalf("unexpected focus, cleanup, metadata wait or provider fallback:\n%s", calls)
			}
		})
	}
}
