package app

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/pibridge"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

func validPiPromptRequest(message map[string]any) bool {
	for key := range message {
		switch key {
		case "type", "action", "protocol", "request_id", "target", "pane_id", "server_session_id", "client_id", "text", "pi_prompt_lease", "_server_received_at", "_server_sequence":
		default:
			return false
		}
	}
	return true
}

func runtimePiPromptIdentity(ctx context.Context, reader paneProcessReader, socket, pane, session string) (pibridge.Identity, error) {
	instance, err := pibridge.Instance(socket)
	if err != nil {
		return pibridge.Identity{}, err
	}
	info, err := reader.PaneProcessInfoFresh(ctx, pane)
	if err != nil {
		return pibridge.Identity{}, err
	}
	pid, err := foregroundRoot(info, pane)
	if err != nil {
		return pibridge.Identity{}, err
	}
	return pibridge.Identity{Instance: instance, Pane: pane, Session: session, PID: pid}, nil
}

func (s *Server) handlePiPrompt(ctx context.Context, client *transport.ClientConn, inbound protocol.Inbound, message map[string]any, admitted func()) *coordinator.CommandResult {
	result := &coordinator.CommandResult{RequestID: inbound.RequestID, Action: inbound.Type, PaneID: inbound.PaneID, Phase: "not_started", Error: "Pi prompt capability is unavailable for this target"}
	agent, exists := s.state.Agent(inbound.PaneID)
	if !exists || !coordinator.PiPromptEligible(agent) || inbound.Target == nil || inbound.RequestID == "" || !validPiPromptRequest(message) {
		return result
	}
	targetBytes, err := json.Marshal(inbound.Target)
	if err != nil {
		return result
	}
	targetKey := string(targetBytes)
	current := func() error {
		if err := herdr.CheckDispatch(ctx); err != nil {
			return err
		}
		latest, ok := s.state.Agent(inbound.PaneID)
		if !ok || !sameSlashTarget(agent, latest) || !coordinator.PiPromptEligible(latest) {
			return errors.New("Pi prompt target changed")
		}
		return nil
	}
	if err := current(); err != nil {
		return result
	}
	if inbound.Type == "get_pi_prompt_capability" {
		identity, err := runtimePiPromptIdentity(ctx, s.herdrC, s.cfg.SocketPath, agent.PaneID, agent.SessionID)
		if err != nil {
			return result
		}
		response, _, err := pibridge.Prompt(ctx, pibridge.PromptLease{Identity: identity, Connection: client.ID(), TargetKey: targetKey}, "", "")
		if err != nil {
			return result
		}
		after, err := runtimePiPromptIdentity(ctx, s.herdrC, s.cfg.SocketPath, agent.PaneID, agent.SessionID)
		if err != nil || identity != after || current() != nil {
			return result
		}
		result.OK, result.Phase, result.Error = true, "completed", ""
		result.Data = map[string]any{"capability": pibridge.PromptCapability, "lease": response.PromptLease}
		return result
	}
	var lease pibridge.PromptLease
	if json.Unmarshal(inbound.PiPromptLease, &lease) != nil || lease.Connection != client.ID() || lease.TargetKey != targetKey || lease.Pane != agent.PaneID || lease.Session != agent.SessionID || !pibridge.ValidPromptText(inbound.Text) {
		return result
	}
	return s.dispatcher.HandlePiPrompt(ctx, inbound.RequestID, inbound.PaneID, admitted, func(effectCtx context.Context) *coordinator.CommandResult {
		if current() != nil {
			return result
		}
		identity, err := runtimePiPromptIdentity(effectCtx, s.herdrC, s.cfg.SocketPath, agent.PaneID, agent.SessionID)
		if err != nil || identity != lease.Identity {
			return result
		}
		checked := herdr.WithDispatchCheck(effectCtx, func() error {
			if err := current(); err != nil {
				return err
			}
			latest, err := runtimePiPromptIdentity(effectCtx, s.herdrC, s.cfg.SocketPath, agent.PaneID, agent.SessionID)
			if err != nil || latest != identity {
				return errors.New("Pi process changed")
			}
			return nil
		})
		response, attempted, err := pibridge.Prompt(checked, lease, inbound.RequestID, inbound.Text)
		if !attempted {
			return result
		}
		result.Phase, result.Error = "dispatched_unknown", "Pi send not confirmed"
		result.Data = map[string]any{"dispatched_unknown": true}
		if err == nil {
			result.OK, result.Phase, result.Error = true, "unconfirmed", ""
			result.Data = map[string]any{"sent_at": response.SentAt}
		}
		return result
	})
}
