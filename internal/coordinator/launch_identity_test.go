package coordinator

import "testing"

func TestLaunchIdentityRequiresUnbrokenTerminalDiscovery(t *testing.T) {
	for _, mode := range []string{"discovery", "terminal replacement", "disappearance", "session rollover", "missing terminal", "existing target"} {
		t.Run(mode, func(t *testing.T) {
			state := NewState(testLogger())
			agent := AgentState{PaneID: "pane", RawPaneID: "raw", TerminalID: "terminal", Agent: "claude"}
			commit := func() { cp := agent; state.CommitInventory([]*AgentState{&cp}, state.RevisionCounter()) }
			if mode == "existing target" {
				commit()
			}
			snapshot := state.beginLaunch()
			if mode == "missing terminal" {
				agent.TerminalID = ""
			}
			commit()
			switch mode {
			case "terminal replacement":
				agent.TerminalID = "replacement"
			case "disappearance":
				state.CommitInventory(nil, state.RevisionCounter())
			}
			agent.SessionID = "first-session"
			commit()
			if mode == "session rollover" {
				agent.SessionID = "another-session"
				commit()
			}
			identity := state.finishLaunch(snapshot, "pane")
			want := mode == "discovery" || mode == "existing target"
			if identity.Valid != want {
				t.Fatalf("identity = %+v, want valid %v", identity, want)
			}
			if want && identity.Generation != uint64(state.Generation("pane")) {
				t.Fatal("identity did not follow proven discovery")
			}
		})
	}
}
