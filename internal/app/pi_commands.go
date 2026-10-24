package app

import (
	"context"
	"errors"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/pibridge"
	"github.com/0cv/herdr-mobile-relay/internal/slashcmd"
)

var piDiscoverySlots = make(chan struct{}, 8)

func sameSlashTarget(before, after *coordinator.AgentState) bool {
	return before != nil && after != nil && before.PaneID == after.PaneID &&
		before.ServerSessionID == after.ServerSessionID && before.TerminalID == after.TerminalID &&
		before.Generation == after.Generation && before.SessionID == after.SessionID &&
		before.Cwd == after.Cwd && before.Agent == after.Agent
}

type paneProcessReader interface {
	PaneProcessInfoFresh(context.Context, string) (*herdr.PaneProcessInfo, error)
}

func foregroundRoot(info *herdr.PaneProcessInfo, pane string) (int, error) {
	if info == nil || pane == "" || info.PaneID != pane || info.ShellPID <= 0 ||
		info.ForegroundProcessGroupID <= 0 || info.ForegroundProcessGroupID == info.ShellPID ||
		len(info.ForegroundProcesses) != 1 || info.ForegroundProcesses[0].PID != info.ForegroundProcessGroupID {
		return 0, errors.New("foreground ownership unavailable")
	}
	return info.ForegroundProcessGroupID, nil
}

func runtimePiCatalog(ctx context.Context, reader paneProcessReader, socket, pane, session string) (slashcmd.Catalog, error) {
	select {
	case piDiscoverySlots <- struct{}{}:
		defer func() { <-piDiscoverySlots }()
	default:
		return slashcmd.Catalog{}, errors.New("Pi discovery busy")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if session == "" {
		return slashcmd.Catalog{}, errors.New("missing Pi session identity")
	}
	instance, err := pibridge.Instance(socket)
	if err != nil {
		return slashcmd.Catalog{}, err
	}
	before, err := reader.PaneProcessInfoFresh(ctx, pane)
	if err != nil {
		return slashcmd.Catalog{}, err
	}
	pid, err := foregroundRoot(before, pane)
	if err != nil {
		return slashcmd.Catalog{}, err
	}
	response, err := pibridge.Query(ctx, pibridge.Identity{Instance: instance, Pane: pane, Session: session, PID: pid})
	if err != nil {
		return slashcmd.Catalog{}, err
	}
	after, err := reader.PaneProcessInfoFresh(ctx, pane)
	if err != nil {
		return slashcmd.Catalog{}, err
	}
	currentPID, err := foregroundRoot(after, pane)
	currentInstance, instanceErr := pibridge.Instance(socket)
	if err != nil || instanceErr != nil || currentPID != pid || currentInstance != instance {
		return slashcmd.Catalog{}, errors.New("Pi process replaced")
	}
	return slashcmd.MergePiRuntime(response.Commands, response.Status, response.Truncated), nil
}
