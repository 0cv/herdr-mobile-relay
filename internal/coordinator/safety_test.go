package coordinator

// Regression tests encoding the ADR-001 / §10.3 / §9.8 safety invariants.

import (
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func ts() int64 { return time.Now().UnixMilli() }

// T1 — §10.3: a poll sample that started before a newer authoritative event
// must not overwrite that event's state.
func TestPollDoesNotOverwriteNewerEvent(t *testing.T) {
	s := NewState(testLogger())
	s.CommitInventory([]*AgentState{{PaneID: "p1", Status: "working"}}, s.RevisionCounter())

	// Capture the revision before the event — simulates a poll that started here.
	staleBase := s.RevisionCounter()

	// A UDP event commits the newer, authoritative state.
	s.CommitEvent("p1", "blocked", ts())

	// A poll that began before the event now lands with its stale sample.
	s.CommitInventory([]*AgentState{{PaneID: "p1", Status: "working"}}, staleBase)

	if got := s.DisplayedStatus("p1"); got != "blocked" {
		t.Fatalf("stale poll overwrote newer event: DisplayedStatus = %q, want %q "+
			"(the per-pane revision guard from §10.3 is not enforced)", got, "blocked")
	}
}

// T2 — §10.3 / §16.13: the blocked publication (and its push notification)
// must fire once per blocked cycle, not on every poll while blocked.
func TestBlockedTransitionFiresOncePerCycle(t *testing.T) {
	s := NewState(testLogger())
	var blockedFires atomic.Int32
	s.SetOnTransition(func(_, _, _, status string, _ int64) {
		if status == "blocked" {
			blockedFires.Add(1)
		}
	})

	// Two consecutive polls of a pane that is (and stays) blocked.
	s.CommitInventory([]*AgentState{{PaneID: "p1", Status: "blocked"}}, s.RevisionCounter())
	s.CommitInventory([]*AgentState{{PaneID: "p1", Status: "blocked"}}, s.RevisionCounter())

	if n := blockedFires.Load(); n != 1 {
		t.Fatalf("blocked notification fired %d times across two polls, want 1 "+
			"(no once-per-event guard; notification spam every poll interval)", n)
	}
}

// T3 — §9.8: a working->idle transition marks the pane unread-done, so the
// phone shows "done" until acknowledged.
func TestUnreadCompletionAfterIdle(t *testing.T) {
	s := NewState(testLogger())
	s.CommitInventory([]*AgentState{{PaneID: "p1", Status: "working"}}, s.RevisionCounter())
	s.CommitInventory([]*AgentState{{PaneID: "p1", Status: "idle"}}, s.RevisionCounter())

	if got := s.DisplayedStatus("p1"); got != "done" {
		t.Fatalf("working->idle did not mark unread completion: DisplayedStatus = %q, want %q", got, "done")
	}
}

// T4 — §9.8 / §16.13: completing via "idle" must still produce a completion
// transition the app can turn into a finished notification.
func TestCompletionTransitionFiresOnIdle(t *testing.T) {
	s := NewState(testLogger())
	var completions atomic.Int32
	s.SetOnTransition(func(_, _, _, status string, _ int64) {
		if status != "working" && status != "blocked" {
			completions.Add(1)
		}
	})

	s.CommitInventory([]*AgentState{{PaneID: "p1", Status: "working"}}, s.RevisionCounter())
	s.CommitInventory([]*AgentState{{PaneID: "p1", Status: "idle"}}, s.RevisionCounter())

	if completions.Load() < 1 {
		t.Fatalf("working->idle produced no completion transition, want >=1 " +
			"(finished notifications never fire for idle-completing agents)")
	}
}

// T5 — §10.4: the poller broadcasts the same *AgentState pointers it stored in
// the state map, while UDP CommitEvent mutates those structs. The broadcast is
// marshaled outside the state lock, so the two race. Must be run with -race.
func TestNoRaceBetweenBroadcastAndEvent(t *testing.T) {
	s := NewState(testLogger())
	agents := []*AgentState{{PaneID: "p1", Status: "working"}}
	s.CommitInventory(agents, s.RevisionCounter()) // stores the same pointers the poller would broadcast

	var wg sync.WaitGroup
	wg.Add(2)

	// Simulates poller.onChange -> hub.Broadcast marshaling the shared slice
	// without holding the state lock.
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			_, _ = json.Marshal(agents)
		}
	}()

	// Simulates UDP events mutating the same AgentState the broadcast reads.
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			if i%2 == 0 {
				s.CommitEvent("p1", "blocked", ts())
			} else {
				s.CommitEvent("p1", "working", ts())
			}
		}
	}()

	wg.Wait()
	// Under -race this fails on the concurrent field access; there is no explicit
	// assertion because the race detector is the oracle.
}
