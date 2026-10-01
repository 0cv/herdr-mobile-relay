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
	entered, release, record := filepath.Join(dir, "entered"), filepath.Join(dir, "release"), filepath.Join(dir, "calls")
	bin := writeScript(t, dir, "herdr", fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %q\ncase \"$1 $2\" in\n 'agent start') touch %q; while [ ! -f %q ]; do sleep 0.01; done; printf '%%s\\n' '{\"result\":{\"pane_id\":\"pane-new\"}}' ;;\n 'agent prompt') printf '%%s\\n' '{\"result\":{}}' ;;\nesac\n", record, entered, release))
	state := NewState(testLogger())
	d := NewDispatcher(herdr.NewClient(bin, filepath.Join(dir, "sock")), state, nil, testLogger())
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0600); _ = d.Close(context.Background()) })
	resultCh := make(chan *CommandResult, 1)
	var revoked atomic.Bool
	ctx := herdr.WithDispatchCheck(context.Background(), func() error {
		if revoked.Load() {
			return errors.New("revoked")
		}
		return nil
	})
	message := map[string]any{"action": "agent_start", "request_id": "normal-discovery", "profile_id": "claude", "name": "proj", "cwd": "/tmp", "prompt": "hello"}
	go func() { resultCh <- d.Handle(ctx, message) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(entered); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("start did not enter")
		}
		time.Sleep(time.Millisecond)
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
		if result.Phase != wantPhase || strings.Count(string(data), "agent prompt") != wantPrompts {
			t.Fatalf("launch identity result=%+v generation=%d calls=%s", result, state.Generation("pane-new"), data)
		}
		replay := d.Handle(context.Background(), message)
		if replay.Phase != result.Phase {
			t.Fatalf("duplicate outcome differs: %+v", replay)
		}
		after, err := os.ReadFile(record)
		if err != nil || string(after) != string(data) {
			t.Fatal("duplicate launch dispatched again")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("launch did not finish")
	}
}
