package app

import (
	"context"
	"errors"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/piintegration"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

func (s *Server) recordPiIntegrationFailures(state piintegration.State) {
	for _, profile := range state.Profiles {
		if profile.Status == "refused" {
			s.recordSafeError("Pi integration", errors.New(profile.Reason))
		}
	}
}

func (s *Server) refreshPiIntegration() {
	if s.piIntegration == nil {
		return
	}
	s.recordPiIntegrationFailures(s.piIntegration.Refresh())
}

func validPiIntegrationRequest(message map[string]any, instance string) bool {
	if instance == "" || message["instance"] != instance {
		return false
	}
	if _, ok := message["enabled"].(bool); !ok {
		return false
	}
	for key := range message {
		switch key {
		case "type", "action", "protocol", "request_id", "client_id", "instance", "enabled", "_server_received_at", "_server_sequence":
		default:
			return false
		}
	}
	return true
}

func (s *Server) handlePiIntegration(ctx context.Context, client *transport.ClientConn, inbound protocol.Inbound, message map[string]any) {
	changing := inbound.Type == "set_pi_integration"
	respond := func(ok bool, problem string, data any) {
		phase := "completed"
		if !ok {
			phase = "failed"
		}
		if changing {
			s.sendAuditedCommandResult(client, message, inbound.RequestID, inbound.Type, ok, phase, problem, "", data)
			return
		}
		s.sendCommandResult(client, inbound.RequestID, inbound.Type, ok, phase, problem, "", data)
	}
	if s.piIntegration == nil || herdr.CheckDispatch(ctx) != nil {
		respond(false, "Pi integration is unavailable", nil)
		return
	}
	var state piintegration.State
	if changing {
		if inbound.RequestID == "" || !validPiIntegrationRequest(message, s.piIntegration.Instance()) {
			respond(false, "Pi integration request does not match this relay", nil)
			return
		}
		state = s.piIntegration.Set(message["enabled"].(bool))
		s.recordPiIntegrationFailures(state)
	} else {
		state = s.piIntegration.Status()
	}
	respond(true, "", state)
}
