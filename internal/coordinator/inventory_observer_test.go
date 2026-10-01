package coordinator

import (
	"sync/atomic"
	"testing"
	"time"
)

// The ownership observer writes a file with two fsyncs. It must run after the
// State lock is released, so readers never wait for that disk write.
func TestInventoryObserverRunsWithoutStateLock(t *testing.T) {
	state := NewState(testLogger())
	observed := make(chan map[string]any, 1)
	state.onInventory = func(agents []*AgentState) {
		observed <- state.InventoryStatus()
	}
	agents := []*AgentState{{PaneID: "pane", RawPaneID: "raw", TerminalID: "terminal", Agent: "claude", Status: "idle"}}
	done := make(chan struct{})
	go func() {
		state.CommitInventory(agents, state.RevisionCounter())
		close(done)
	}()
	select {
	case status := <-observed:
		if status["state"] != "ready" {
			t.Fatalf("observer read stale inventory: %v", status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("observer could not read state; the commit lock is still held")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("commit did not finish")
	}
	for _, commit := range []func(){
		func() { state.CommitTopology(agents, nil, state.RevisionCounter()) },
		func() { state.CommitPoll(agents, nil, state.BeginPoll()) },
	} {
		go commit()
		select {
		case <-observed:
		case <-time.After(5 * time.Second):
			t.Fatal("observer could not read state on a different commit path")
		}
	}
}

// Ownership reconciliation deletes records missing from the snapshot it is
// given, so a slow observer must never publish an older snapshot last.
func TestInventoryObserverPublishesNewestSnapshotLast(t *testing.T) {
	state := NewState(testLogger())
	deliveries := make(chan int, 4)
	blocked := make(chan struct{})
	release := make(chan struct{})
	var started atomic.Int32
	state.onInventory = func(agents []*AgentState) {
		if started.Add(1) == 1 {
			close(blocked)
			<-release
		}
		deliveries <- len(agents)
	}
	one := []*AgentState{{PaneID: "pane-1", RawPaneID: "raw-1", TerminalID: "terminal-1", Agent: "claude", Status: "idle"}}
	two := []*AgentState{
		one[0],
		{PaneID: "pane-2", RawPaneID: "raw-2", TerminalID: "terminal-2", Agent: "claude", Status: "idle"},
	}

	go state.CommitInventory(one, state.RevisionCounter())
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("first observer never ran")
	}
	go state.CommitInventory(two, state.RevisionCounter())
	// Give the newer commit time to reach delivery while the older one is stuck.
	time.Sleep(200 * time.Millisecond)
	close(release)

	sizes := make([]int, 0, 2)
	for len(sizes) < 2 {
		select {
		case size := <-deliveries:
			sizes = append(sizes, size)
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d snapshots were delivered: %v", len(sizes), sizes)
		}
	}
	if sizes[len(sizes)-1] != len(two) {
		t.Fatalf("stale snapshot published last: deliveries=%v", sizes)
	}
}
