package coordinator

import (
	"testing"
	"time"
)

func TestTopologyStaleRepollsAreBounded(t *testing.T) {
	state := testState()
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Status: "working"}}, 0)
	poller := NewPoller(nil, state, time.Second, testLogger())

	for retry := 0; retry < maxImmediateTopologyPolls; retry++ {
		poller.handleTopologyStale(state.InventoryStatus())
		select {
		case <-poller.wakeup:
		default:
			t.Fatalf("retry %d did not request an immediate repoll", retry+1)
		}
	}
	poller.handleTopologyStale(state.InventoryStatus())
	select {
	case <-poller.wakeup:
		t.Fatal("topology churn requested an unbounded immediate repoll")
	default:
	}
	status := state.InventoryStatus()
	if status["state"] != "error" || status["error_code"] != "topology_churn" {
		t.Fatalf("inventory status = %+v, want topology degradation", status)
	}
}

func TestSustainedPollFailuresBackOffAndSuccessResets(t *testing.T) {
	state := testState()
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Status: "working"}}, 0)
	poller := NewPoller(nil, state, time.Second, testLogger())

	poller.pollFailures = pollFailureBackoffAfter - 1
	if got := poller.currentInterval(); got != time.Second {
		t.Fatalf("interval before threshold = %v, want %v", got, time.Second)
	}
	poller.pollFailures++
	if got := poller.currentInterval(); got != idlePollInterval {
		t.Fatalf("interval after sustained failures = %v, want %v", got, idlePollInterval)
	}
	poller.pollFailures = 0
	if got := poller.currentInterval(); got != time.Second {
		t.Fatalf("interval after success = %v, want %v", got, time.Second)
	}
}
