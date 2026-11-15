package app

import (
	"strings"

	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

func (s *Server) authorizeDeviceAction(client *transport.ClientConn, action protocol.ActionMetadata, deviceID string) *protocol.ApiError {
	identity, authenticated := client.Identity()
	if authenticated && s.deviceAuth != nil {
		credential, current := s.deviceAuth.AuthorizeCredential(identity.CredentialID, identity.CredentialVersion)
		if !current || credential.DeviceID != identity.DeviceID {
			apiErr := protocol.NewApiError(protocol.ErrorReaderDenied, map[string]any{
				"operation": action.Operation,
				"reason":    "credential_revoked",
			})
			return &apiErr
		}
		identity.Role = string(credential.Role)
		identity.Locale = credential.Locale
	}
	return authorizeAuthenticatedIdentity(identity, authenticated, action, deviceID)
}

func authorizeAuthenticatedIdentity(identity transport.AuthenticatedIdentity, authenticated bool, action protocol.ActionMetadata, deviceID string) *protocol.ApiError {
	if action.Operation == "get_pi_integration" && !authenticated {
		apiErr := protocol.NewApiError(protocol.ErrorReaderDenied, map[string]any{"operation": action.Operation})
		return &apiErr
	}
	if action.Operation == "get_pi_prompt_capability" || action.Operation == "send_pi_prompt_unconfirmed" || action.Operation == "set_pi_integration" {
		if !authenticated || identity.Role != string(protocol.RoleController) {
			apiErr := protocol.NewApiError(protocol.ErrorReaderDenied, map[string]any{"operation": action.Operation})
			return &apiErr
		}
	}
	deviceBoundPush := false
	switch action.Operation {
	case "push_open_ref", "push_policy_get", "push_policy_set", "push_snooze", "push_subscribe",
		"push_test_device", "push_unsubscribe", "push_viewed_pane":
		deviceBoundPush = true
	}
	if deviceBoundPush {
		// Every device-bound push operation acts on the caller's own
		// subscription and policy, so a reader is as entitled to it as a
		// controller. Delivery tests are rate limited per device instead of
		// gated by role.
		if authenticated && strings.TrimSpace(identity.DeviceID) != "" {
			return nil
		}
		apiErr := protocol.NewApiError(protocol.ErrorReaderDenied, map[string]any{"operation": action.Operation})
		return &apiErr
	}
	if !authenticated || action.Class == protocol.ActionReadOnly || identity.Role == string(protocol.RoleController) {
		return nil
	}
	if action.Operation == "revoke_device" && deviceID != "" && deviceID == identity.DeviceID {
		return nil
	}
	apiErr := protocol.NewApiError(protocol.ErrorReaderDenied, map[string]any{"operation": action.Operation})
	return &apiErr
}

func validateExactPaneTarget(state *coordinator.State, inbound protocol.Inbound, authenticated bool) *protocol.ApiError {
	target := inbound.Target
	if inbound.PaneID == "" {
		return nil
	}
	if inbound.Type == "unwatch_pane" || inbound.Type == "release_pane_size" || inbound.Type == "cancel_speech" {
		if target != nil && target.PaneID != inbound.PaneID {
			apiErr := protocol.NewApiError(protocol.ErrorInvalidRequest, map[string]any{"field": "target.pane_id"})
			return &apiErr
		}
		// These operations clean up owner-scoped state already recorded by
		// client id and pane id or speech request id. A replaced pane must not
		// strand the old watch, size lease, or synthesis merely because its
		// exact target is now stale.
		return nil
	}
	if target == nil {
		if !authenticated {
			return nil
		}
		apiErr := protocol.NewApiError(protocol.ErrorInvalidRequest, map[string]any{"field": "target"})
		return &apiErr
	}
	if target.PaneID != inbound.PaneID {
		apiErr := protocol.NewApiError(protocol.ErrorInvalidRequest, map[string]any{"field": "target.pane_id"})
		return &apiErr
	}
	agent, ok := state.Agent(inbound.PaneID)
	if !ok {
		apiErr := protocol.NewApiError(protocol.ErrorInvalidRequest, map[string]any{"field": "target"})
		return &apiErr
	}
	if !targetMatchesAgent(*target, agent) {
		apiErr := protocol.NewApiError(protocol.ErrorInvalidRequest, map[string]any{"field": "target"})
		return &apiErr
	}
	return nil
}

func targetMatchesAgent(target protocol.TargetRef, agent *coordinator.AgentState) bool {
	return target.ServerSessionID == protocol.PrimaryServerSessionID &&
		target.PaneID == agent.PaneID &&
		target.TerminalID != "" && target.TerminalID == agent.TerminalID &&
		target.Generation == agent.Generation &&
		target.AgentSessionID == agent.SessionID
}
