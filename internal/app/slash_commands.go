package app

import (
	"encoding/json"

	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/slashcmd"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

func (s *Server) handleListSlashCommands(client *transport.ClientConn, inbound protocol.Inbound) {
	paneID := inbound.PaneID
	reply := s.reply(client, inbound.RequestID, "list_slash_commands", paneID)
	if paneID == "" {
		reply.fail("Agent is required")
		return
	}
	activeAgent, ok := s.state.Agent(paneID)
	if !ok {
		reply.fail("Agent pane not found")
		return
	}
	agent, cwd := activeAgent.Agent, activeAgent.Cwd
	project := projectContextForAgent(activeAgent)
	profileID := s.profiles.ResolvePane(paneID, agent)
	skillDirs, commandFormat, suppressNative := s.profiles.CommandDiscovery(profileID)
	location := s.conversationM.LocateWithProject(agent, project, activeAgent.SessionID)
	agentDir := locatedAgentDir(s.home, agent, location)
	catalog := slashcmd.CatalogFor(profileID, agent, slashcmd.DiscoverContext{
		AgentDir:       agentDir,
		Cwd:            cwd,
		Home:           s.home,
		SkillDirs:      skillDirs,
		CommandFormat:  commandFormat,
		SuppressNative: suppressNative,
	})
	if slashcmd.IsPi(profileID, agent) && !suppressNative && commandFormat == "" {
		catalog.Status = "partial"
		if runtime, err := runtimePiCatalog(client.Context(), s.herdrC, s.cfg.SocketPath, paneID, activeAgent.SessionID); err == nil {
			if runtime.Status == "available" || runtime.Status == "partial" {
				catalog = runtime
			} else {
				catalog.Status = runtime.Status
			}
		}
	}
	currentAgent, currentExists := s.state.Agent(paneID)
	if !currentExists || !sameSlashTarget(activeAgent, currentAgent) {
		reply.fail("The agent pane was replaced while commands were being listed")
		return
	}
	catalog = fitSlashCommandCatalog(slashcmd.Revise(catalog), inbound.RequestID, "list_slash_commands", paneID)
	reply.complete(catalog)
}

func fitSlashCommandCatalog(catalog slashcmd.Catalog, requestID, action, paneID string) slashcmd.Catalog {
	if slashCommandResultFits(catalog, requestID, action, paneID) {
		return catalog
	}

	commands := catalog.Commands
	low, high := 0, len(commands)
	for low < high {
		mid := low + (high-low+1)/2
		candidate := trimSlashCatalog(catalog, mid)
		if slashCommandResultFits(candidate, requestID, action, paneID) {
			low = mid
		} else {
			high = mid - 1
		}
	}
	return trimSlashCatalog(catalog, low)
}

func trimSlashCatalog(catalog slashcmd.Catalog, count int) slashcmd.Catalog {
	catalog.Commands = catalog.Commands[:count]
	catalog.Truncated = true
	metadata := make(map[string]slashcmd.Metadata)
	for _, command := range catalog.Commands {
		if value, ok := catalog.Metadata[command.Command]; ok {
			metadata[command.Command] = value
		}
	}
	catalog.Metadata = metadata
	return slashcmd.Revise(catalog)
}

func slashCommandResultFits(catalog slashcmd.Catalog, requestID, action, paneID string) bool {
	message := coordinator.CommandResultMessage(&coordinator.CommandResult{
		RequestID: requestID,
		Action:    action,
		OK:        true,
		Phase:     "completed",
		PaneID:    paneID,
		Data:      catalog,
	})
	encoded, err := json.Marshal(message)
	return err == nil && len(encoded) <= transport.MaxOutboundMessageBytes
}
