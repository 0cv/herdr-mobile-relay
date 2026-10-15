package app

import (
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/piscroll"
)

func sameSlashTarget(before, after *coordinator.AgentState) bool {
	return before != nil && after != nil && before.PaneID == after.PaneID &&
		before.ServerSessionID == after.ServerSessionID && before.TerminalID == after.TerminalID &&
		before.Generation == after.Generation && before.SessionID == after.SessionID &&
		before.Cwd == after.Cwd && before.Agent == after.Agent
}

func foregroundRoot(info *herdr.PaneProcessInfo, pane string) (int, error) {
	return piscroll.Foreground(info, pane)
}
