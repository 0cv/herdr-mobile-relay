package coordinator

import (
	"context"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

type ownershipCoreBackend struct {
	*coreLaunchBackend
	panes []herdr.Pane
}

func (b *ownershipCoreBackend) PaneList(context.Context) ([]herdr.Pane, error) {
	return b.panes, nil
}

func TestCoreOwnershipBeforeAgentReports(t *testing.T) {
	f := newLaunchFixture(t)
	p := herdr.Pane{ID: f.target.PaneID, TerminalID: f.target.TerminalID, TabID: f.target.TabID, WorkspaceID: f.target.WorkspaceID}
	b := &ownershipCoreBackend{coreLaunchBackend: &coreLaunchBackend{Backend: f.dispatcher.herdr}, panes: []herdr.Pane{p}}
	f.dispatcher.herdr = b
	f.dispatcher.SetProfiles(f.dispatcher.profiles)
	if err := f.dispatcher.profiles.RememberVerified(f.target, "claude"); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		f.dispatcher.state.CommitInventory([]*AgentState{}, 0)
	}
	if got := f.dispatcher.profiles.ResolveOwnedPane(p.ID); got != "claude" {
		t.Fatalf("ownership before status.app = %q", got)
	}
	b.panes = nil
	for range 2 {
		f.dispatcher.state.CommitInventory([]*AgentState{}, 0)
	}
	if f.dispatcher.profiles.OwnsTarget(f.target, "claude") {
		t.Fatal("removed terminal retained ownership")
	}
}
