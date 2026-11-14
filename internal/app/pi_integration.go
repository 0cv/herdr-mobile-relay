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
	if s.cfg.Backend == "hc" || s.piIntegration == nil {
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
	reply := s.reply(client, inbound.RequestID, inbound.Type, "")
	if changing {
		reply = s.auditedReply(client, message, inbound.RequestID, inbound.Type, "")
	}
	if changing && s.cfg.Backend == "hc" {
		reply.fail("Pi integration is managed by the TUI relay, not the hc backend")
		return
	}
	if s.piIntegration == nil || herdr.CheckDispatch(ctx) != nil {
		reply.fail("Pi integration is unavailable")
		return
	}
	var state piintegration.State
	if changing {
		if inbound.RequestID == "" || !validPiIntegrationRequest(message, s.piIntegration.Instance()) {
			reply.fail("Pi integration request does not match this relay")
			return
		}
		state = s.piIntegration.Set(message["enabled"].(bool))
		s.recordPiIntegrationFailures(state)
	} else {
		state = s.piIntegration.Status()
	}
	reply.complete(state)
}
