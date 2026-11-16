package coordinator

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/profiles"
)

// startSettleBackstop only keeps a hung start from stalling the suite. The
// start itself settles at its own deadline on the real clock, which can be
// seconds away for the cancelled and timeout outcomes.
const startSettleBackstop = 30 * time.Second

func TestHermesLaunchReceiptFollowsStartNotTabCreation(t *testing.T) {
	for _, outcome := range []string{"completed", "refused", "malformed", "cancelled", "timeout"} {
		t.Run(outcome, func(t *testing.T) {
			env := newLaunchEnv(t, "hermes")
			client := newLaunchFake()
			client.target.WorkspaceID = "workspace"
			client.workspaces = []herdr.Workspace{{ID: "workspace"}}
			gate := newReleaseGate()
			var startErr error
			switch outcome {
			case "refused":
				startErr = refusalByCode("agent_pane_busy")
			case "malformed":
				startErr = startedFailure("response has no result envelope")
			}
			client.setHook("StartAgent", gate.hook(func(context.Context, fakeCall) error { return startErr }))
			d := newTestDispatcher(t, client, NewState(testLogger()))
			d.SetProfiles(profiles.NewResolver(env.config, nil))
			d.lifecycle.home = env.home
			stopAfterRefusals(d.lifecycle, 1)
			t.Cleanup(gate.open)
			budget := 8 * time.Second
			if outcome == "timeout" {
				budget = agentStartResponseReserve + 2*time.Second
			}
			ctx, cancel := context.WithTimeout(t.Context(), budget)
			defer cancel()
			message := map[string]any{"type": "agent_start", "request_id": "hermes-launch", "profile_id": "hermes", "workspace_id": "workspace", "name": "development-hermes", "cwd": env.cwd}
			pending := make(chan *CommandResult, 1)
			go func() { pending <- d.Handle(ctx, message) }()
			select {
			case <-gate.entered:
			case result := <-pending:
				t.Fatalf("start returned before fake Herdr started: %+v", result)
			case <-ctx.Done():
				t.Fatal("fake Herdr did not receive agent start")
			}
			select {
			case result := <-pending:
				t.Fatalf("tab creation was incorrectly treated as start completion: %+v", result)
			default:
			}
			if outcome == "cancelled" {
				cancel()
			} else if outcome != "timeout" {
				gate.open()
			}
			var result *CommandResult
			select {
			case result = <-pending:
			case <-time.After(startSettleBackstop):
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
			starts := client.callsTo("StartAgent")
			if len(starts) != 1 || !reflect.DeepEqual(starts[0].Args[:3], []any{"development-hermes", "hermes", "pane-new"}) || client.count("TabCreate") != 1 {
				t.Fatalf("unexpected launch calls: %+v", client.recorded())
			}
			if client.count("StopPane") != 0 || client.count("AgentGet") != 0 || client.count("PaneRun") != 0 {
				t.Fatalf("unexpected cleanup, metadata wait or provider fallback: %+v", client.recorded())
			}
		})
	}
}
