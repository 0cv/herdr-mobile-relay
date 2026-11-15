package coordinator

import (
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func TestCoreOwnershipBeforeAgentReports(t *testing.T) {
	f := newLaunchFixture(t)
	p := herdr.Pane{ID: f.target.PaneID, TerminalID: f.target.TerminalID, TabID: f.target.TabID, WorkspaceID: f.target.WorkspaceID}
	f.client.enableOneStepLaunch()
	f.client.update(func(b *fakeBackend) { b.panes = []herdr.Pane{p} })
	f.dispatcher.SetProfiles(f.dispatcher.profiles)
	if err := f.dispatcher.profiles.RememberVerified(f.target, "claude"); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		f.dispatcher.state.CommitInventory([]*AgentState{}, 0)
	}
	if got := f.dispatcher.profiles.ResolvePane(p.ID, ""); got != "claude" {
		t.Fatalf("ownership before status.app = %q", got)
	}
	f.client.update(func(b *fakeBackend) { b.panes = nil })
	for range 2 {
		f.dispatcher.state.CommitInventory([]*AgentState{}, 0)
	}
	if f.dispatcher.profiles.OwnsTarget(f.target, "claude") {
		t.Fatal("removed terminal retained ownership")
	}
}
