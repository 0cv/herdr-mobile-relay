package coordinator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
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
	dir := t.TempDir()
	entered, release := filepath.Join(dir, "entered"), filepath.Join(dir, "release")
	f := newLaunchFixtureWithOptions(t, launchFixtureOptions{start: fmt.Sprintf(": > %q; while [ ! -f %q ]; do /bin/sleep 0.01; done", entered, release)})
	d, state, record := f.dispatcher, f.dispatcher.state, f.calls
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0600) })
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
	message := map[string]any{"action": "agent_start", "request_id": "normal-discovery", "profile_id": "claude", "name": "proj", "cwd": f.cwd, "prompt": "hello"}
	go func() { resultCh <- d.Handle(ctx, message) }()
	for {
		if _, err := os.Stat(entered); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("start did not enter")
		case <-time.After(time.Millisecond):
		}
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
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-resultCh:
		data, err := os.ReadFile(record)
		if err != nil {
			t.Fatal(err)
		}
		wantPhase, wantPrompts := "completed", 1
		if mode == "replacement" || mode == "disappearance" || mode == "revocation" {
			wantPhase, wantPrompts = "completed_with_warning", 0
		}
		if !result.OK || result.PaneID != f.target.PaneID || result.Phase != wantPhase || strings.Count(string(data), "agent prompt") != wantPrompts {
			t.Fatalf("launch identity result=%+v generation=%d calls=%s", result, state.Generation("pane-new"), data)
		}
		if wantPhase == "completed" {
			f.assertLaunch(t, result)
		}
		f.assertCalls(t, wantPrompts)
		replay := d.Handle(ctx, message)
		if replay.Phase != result.Phase {
			t.Fatalf("duplicate outcome differs: %+v", replay)
		}
		after, err := os.ReadFile(record)
		if err != nil || string(after) != string(data) {
			t.Fatal("duplicate launch dispatched again")
		}
	case <-ctx.Done():
		t.Fatal("launch did not finish")
	}
}
