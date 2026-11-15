package coordinator

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestIdentifiedPaneReadDoesNotReuseEarlierInFlightObservation(t *testing.T) {
	client := newFakeBackend()
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	var reads atomic.Int32
	client.setHook("ReadPaneRecent", func(ctx context.Context, _ fakeCall) error {
		if reads.Add(1) > 1 {
			client.update(func(b *fakeBackend) { b.contents["pane-1"] = "new observation\n" })
			return nil
		}
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		client.update(func(b *fakeBackend) { b.contents["pane-1"] = "earlier observation\n" })
		return nil
	})
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Agent: "pi", Status: "idle"}}, state.RevisionCounter())
	dispatcher := newTestDispatcher(t, client, state)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	first := make(chan map[string]any, 1)
	go func() {
		first <- dispatcher.HandleReadPane(ctx, map[string]any{"pane_id": "pane-1", "format": "ansi", "request_id": "before-send"})
	}()
	defer func() {
		unblock()
		select {
		case response := <-first:
			if response["content"] != "earlier observation\n" {
				t.Errorf("earlier read = %#v", response)
			}
		case <-ctx.Done():
			t.Error("earlier read did not finish after release")
		}
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("earlier read did not start")
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
