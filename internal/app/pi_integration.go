package app

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

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

var (
	piPromptKeys      = []string{"type", "protocol", "request_id", "target", "pane_id", "server_session_id", "client_id", "text", "pi_prompt_lease"}
	piIntegrationKeys = []string{"type", "protocol", "request_id", "client_id", "instance", "enabled"}
)

func piFrameKeysAllowed(inbound protocol.Inbound) bool {
	allowed := piPromptKeys
	if inbound.Type == "set_pi_integration" {
		allowed = piIntegrationKeys
	}
	var keys map[string]json.RawMessage
	if json.Unmarshal(inbound.Raw, &keys) != nil {
		return false
	}
	for key := range keys {
		if !slices.Contains(allowed, key) {
			return false
		}
	}
	return true
}

func (s *Server) handlePiIntegration(ctx context.Context, client *transport.ClientConn, inbound protocol.Inbound) {
	changing := inbound.Type == "set_pi_integration"
	reply := s.reply(client, inbound.RequestID, inbound.Type, "")
	if changing {
		reply = s.auditedReply(client, inbound, "")
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
		instance := s.piIntegration.Instance()
		if inbound.RequestID == "" || instance == "" || inbound.Instance != instance || inbound.Enabled == nil || !piFrameKeysAllowed(inbound) {
			reply.fail("Pi integration request does not match this relay")
			return
		}
		state = s.piIntegration.Set(*inbound.Enabled)
		s.recordPiIntegrationFailures(state)
	} else {
		state = s.piIntegration.Status()
	}
	reply.complete(state)
}
