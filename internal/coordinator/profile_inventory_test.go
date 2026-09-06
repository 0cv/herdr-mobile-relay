package coordinator

import "testing"

func TestProfileOwnershipChangeAdvancesContentRevision(t *testing.T) {
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{PaneID: "pane", Agent: "copilot", ProfileID: "personal", Status: "idle"}}, state.RevisionCounter())
	before := state.ContentRevision("pane")
	state.CommitInventory([]*AgentState{{PaneID: "pane", Agent: "copilot", ProfileID: "emu", Status: "idle"}}, state.RevisionCounter())
	after := state.ContentRevision("pane")
	if after != before+1 {
		t.Fatalf("content revision = %d, want %d", after, before+1)
	}
	agent, ok := state.Agent("pane")
	if !ok || agent.ProfileID != "emu" {
		t.Fatalf("profile ownership = %+v, %v", agent, ok)
	}
}
