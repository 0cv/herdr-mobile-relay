package app

import (
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
)

// Deny unsupported remote operations before any local filesystem, upload,
// transcript, profile, resize, or mutation handler can interpret remote data.
func remoteRequestDenied(in protocol.Inbound) bool {
	remotePane := herdr.IsRemoteID(in.PaneID)
	if in.Target != nil {
		remotePane = remotePane || herdr.IsRemoteID(in.Target.PaneID) || herdr.IsRemoteID(in.Target.TerminalID)
	}
	remoteWorkspace := herdr.IsRemoteID(in.WorkspaceID) || herdr.IsRemoteID(in.BeforeWorkspaceID)
	for _, id := range in.WorkspaceIDs {
		remoteWorkspace = remoteWorkspace || herdr.IsRemoteID(id)
	}
	for _, id := range in.ExpectedWorkspaceIDs {
		remoteWorkspace = remoteWorkspace || herdr.IsRemoteID(id)
	}
	if remoteWorkspace {
		return true
	}
	if !remotePane {
		return false
	}
	switch in.Type {
	case "read_pane", "watch_pane", "unwatch_pane", "pane_applied":
		return false
	default:
		return true
	}
}
