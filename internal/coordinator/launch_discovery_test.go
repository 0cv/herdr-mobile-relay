package coordinator

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
)

func TestReviewInitialPromptSurvivesNormalSessionDiscovery(t *testing.T) {
	runLaunchDiscovery(t, "discovery")
}

func TestLaunchDiscoveryRejectsReplacementAndRevocation(t *testing.T) {
	for _, mode := range []string{"replacement", "disappearance", "revocation", "scheduler observed first"} {
		t.Run(mode, func(t *testing.T) { runLaunchDiscovery(t, mode) })
	}
}

func runLaunchDiscovery(t *testing.T, mode string) {
	gate := newReleaseGate()
	f := newLaunchFixtureWithOptions(t, launchFixtureOptions{start: gate.hook(nil)})
	d, state := f.dispatcher, f.dispatcher.state
	t.Cleanup(gate.open)
	resultCh := make(chan *CommandResult, 1)
	var revoked atomic.Bool
	waitCtx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	ctx := herdr.WithDispatchCheck(waitCtx, func() error {
		if revoked.Load() {
			return errors.New("revoked")
		}
		return nil
	})
	message := protocol.Inbound{Type: "agent_start", RequestID: "normal-discovery", ProfileID: "claude", Name: "proj", Cwd: f.cwd, Prompt: "hello"}
	go func() { resultCh <- d.Handle(ctx, message) }()
	select {
	case <-gate.entered:
	case <-ctx.Done():
		t.Fatal("start did not enter")
	}
	state.CommitInventory([]*AgentState{{PaneID: "pane-new", RawPaneID: "raw-new", TerminalID: "terminal-new", Agent: "claude", Status: "idle"}}, state.RevisionCounter())
	terminal := "terminal-new"
	if mode == "replacement" {
		terminal = "replacement"
	}
	if mode == "disappearance" {
		state.CommitInventory(nil, state.RevisionCounter())
	}
	if mode == "scheduler observed first" {
		d.PruneSlots(map[string]bool{"pane-new": true})
	}
	state.CommitInventory([]*AgentState{{PaneID: "pane-new", RawPaneID: "raw-new", TerminalID: terminal, Agent: "claude", Status: "idle", SessionID: "native-session"}}, state.RevisionCounter())
	if mode == "revocation" {
		revoked.Store(true)
	}
	gate.open()
	select {
	case result := <-resultCh:
		calls := f.client.recorded()
		wantPhase, wantPrompts := "completed", 1
		if mode == "replacement" || mode == "disappearance" || mode == "revocation" {
			wantPhase, wantPrompts = "completed_with_warning", 0
		}
		if !result.OK || result.PaneID != f.target.PaneID || result.Phase != wantPhase || f.client.count("Prompt") != wantPrompts {
			t.Fatalf("launch identity result=%+v generation=%d calls=%+v", result, state.Generation("pane-new"), calls)
		}
		if wantPhase == "completed" {
			f.assertLaunch(t, result)
		}
		f.assertCalls(t, wantPrompts)
		replay := d.Handle(ctx, message)
		if replay.Phase != result.Phase {
			t.Fatalf("duplicate outcome differs: %+v", replay)
		}
		if !reflect.DeepEqual(f.client.recorded(), calls) {
			t.Fatal("duplicate launch dispatched again")
		}
	case <-ctx.Done():
		t.Fatal("launch did not finish")
	}
}
