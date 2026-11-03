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

func (s *Server) enrichPiSessions(ctx context.Context, agents []*coordinator.AgentState) {
	if s.cfg.Backend != "hc" {
		return
	}
	s.piSessionsMu.Lock()
	cachedSessions := make(map[string]piSessionIdentity, len(s.piSessions))
	for terminal, identity := range s.piSessions {
		cachedSessions[terminal] = identity
	}
	s.piSessionsMu.Unlock()
	current := make(map[string]piSessionIdentity)
	for _, a := range agents {
		if a == nil || !slashcmd.IsPi("", a.Agent) || a.Session != "" {
			continue
		}
		func() {
			ctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
			defer cancel()
			pid := 0
			var err error
			defer func() {
				if err != nil && a.TerminalID != "" {
					if cached := cachedSessions[a.TerminalID]; pid == 0 || cached.pid == pid {
						a.Session = cached.session
						if cached.session != "" {
							current[a.TerminalID] = cached
						}
					}
				}
			}()
			info, err := s.herdrC.PaneProcessInfoFresh(ctx, a.PaneID)
			if err != nil {
				return
			}
			pid, err = foregroundRoot(info, a.PaneID)
			if err != nil {
				return
			}
			instance, err := pibridge.Instance(s.cfg.SocketPath)
			session := ""
			if err == nil {
				session, err = pibridge.Identify(ctx, instance, a.PaneID, pid)
			}
			if err != nil {
				return
			}
			a.Session = session
			if session != "" && a.TerminalID != "" {
				current[a.TerminalID] = piSessionIdentity{pid: pid, session: session}
			}
		}()
	}
	s.piSessionsMu.Lock()
	s.piSessions = current
	s.piSessionsMu.Unlock()
}

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
	if info == nil || pane == "" || info.PaneID != pane || info.ShellPID < 0 ||
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
