package coordinator

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func TestIdentifiedPaneReadDoesNotReuseEarlierInFlightObservation(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "first-started")
	release := filepath.Join(dir, "release")
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"if mkdir '"+filepath.Join(dir, "first")+"' 2>/dev/null; then\n"+
		"  touch '"+started+"'\n"+
		"  while [ ! -f '"+release+"' ]; do sleep 0.01; done\n"+
		"  printf 'earlier observation\\n'\n"+
		"else\n"+
		"  printf 'new observation\\n'\n"+
		"fi\n")
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Agent: "pi", Status: "idle"}}, state.RevisionCounter())
	dispatcher := NewDispatcher(herdr.NewClient(bin, filepath.Join(dir, "missing.sock")), state, nil, testLogger())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	first := make(chan map[string]any, 1)
	go func() {
		first <- dispatcher.HandleReadPane(ctx, map[string]any{"pane_id": "pane-1", "format": "ansi", "request_id": "before-send"})
	}()
	defer func() {
		if err := os.WriteFile(release, nil, 0o600); err != nil {
			t.Error(err)
		}
		select {
		case response := <-first:
			if response["content"] != "earlier observation\n" {
				t.Errorf("earlier read = %#v", response)
			}
		case <-ctx.Done():
			t.Error("earlier read did not finish after release")
		}
	}()
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("earlier read did not start")
		case <-time.After(5 * time.Millisecond):
		}
	}
	second := make(chan map[string]any, 1)
	go func() {
		second <- dispatcher.HandleReadPane(ctx, map[string]any{"pane_id": "pane-1", "format": "ansi", "request_id": "after-unknown-outcome"})
	}()
	select {
	case response := <-second:
		if response["content"] != "new observation\n" {
			t.Fatalf("new read reused earlier observation: %#v", response)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("new identified read waited on the earlier in-flight observation")
	}
}
