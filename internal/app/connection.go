package app

import (
	"slices"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/hermesprofiles"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

func (s *Server) effectiveCapabilitiesFor(herdrStatus herdr.ServerStatus) []string {
	capabilities := append([]string(nil), protocol.Capabilities...)
	if s.cfg.Backend == "hc" {
		capabilities = slices.DeleteFunc(capabilities, func(capability string) bool { return capability == "self_update" })
	}
	capabilities = append(capabilities, "pi_prompt_unconfirmed")
	// Never the legacy pane_size_lease names: apps up to 0.22.7 lease
	// automatically when they see them.
	if s.paneSizeM.Available() {
		capabilities = append(capabilities, protocol.PaneSizeOptInCapability, protocol.PaneSizeOptInRowsCapability)
	}
	if s.pushM != nil {
		capabilities = append(capabilities, "typed_push", "push_policy")
	}
	if s.clipboardRead != nil {
		capabilities = append(capabilities, protocol.AgentResponseCopyCapability)
	}
	speechStatus := s.speechStatus()
	if len(s.rememberSpeechLanguages(speechStatus.Languages)) > 0 {
		capabilities = append(capabilities, protocol.SpeechSynthesisCapability)
	}
	if speechStatus.ManagementSupported {
		capabilities = append(capabilities, protocol.SpeechVoiceManagementCapability)
	}
	if herdrStatus.Supports(herdr.FeaturePaneRead) {
		capabilities = append(capabilities, "pane_realtime_delta")
	}
	for _, feature := range []string{
		herdr.FeatureWorkspaceManagement,
		herdr.FeatureWorkspaceRename,
		herdr.FeatureWorkspaceReorder,
		herdr.FeatureWorktreeManagement,
		herdr.FeatureTabRename,
		herdr.FeatureAgentLaunch,
		herdr.FeatureManualTerminalInput,
		herdr.FeatureSecretInput,
		herdr.FeatureSemanticInput,
		herdr.FeaturePiIntegration,
	} {
		if herdrStatus.Supports(feature) {
			capabilities = append(capabilities, feature)
		}
	}
	if herdrStatus.Supports(herdr.FeatureTabMove) {
		capabilities = append(capabilities, "tab_reorder")
	}
	if herdrStatus.Supports(herdr.FeatureWorkspaceMoveBlock) {
		capabilities = append(capabilities, "workspace_reorder_block")
	}
	if s.cfg.Backend != "hc" && s.appDeployM.State().Configured {
		capabilities = append(capabilities, "app_deploy")
	}
	if s.hybrid != nil && s.hybrid.directEnabled() {
		capabilities = append(capabilities, "webrtc_direct")
	}
	if s.deviceAuth != nil {
		capabilities = append(capabilities, "device_management")
	}
	return capabilities
}

func (s *Server) sendConnectionSnapshot(client *transport.ClientConn) {
	vapidPublicKey := ""
	if s.pushM != nil {
		vapidPublicKey = s.pushM.VAPIDPublicKey()
	}
	committed := s.committedInventorySnapshot()
	s.observeInventoryPublication("registration")
	inventory := committed.status
	herdrCapabilityStatus := s.herdrC.CapabilityStatus()
	capabilities := s.effectiveCapabilitiesFor(herdrCapabilityStatus)
	speechStatus := s.speechStatus()
	speechLanguages := s.rememberSpeechLanguages(speechStatus.Languages)
	s.hub.Send(client, protocol.PushConfig{
		Type:            "push_config",
		VAPIDPublicKey:  vapidPublicKey,
		Host:            s.hostname,
		Home:            s.home,
		Protocol:        protocol.Version,
		Version:         s.version,
		ReleaseVersion:  s.version,
		Revision:        s.revision,
		Update:          s.updateM.State(),
		AppDeploy:       s.appDeployM.State(),
		Capabilities:    capabilities,
		HerdrStatus:     herdrCapabilityStatus,
		SpeechLanguages: speechLanguages,
		Inventory:       inventory,
		AgentProfiles:   s.profiles.Profiles(),
		HermesProfiles:  hermesprofiles.List(s.home),
		Hybrid:          s.hybridDescriptor(),
	})
	s.hub.Send(client, map[string]any{
		"type":   "agents",
		"agents": committed.agents,
	})
	s.hub.Send(client, map[string]any{
		"type":       "workspaces",
		"workspaces": committed.workspaces,
	})
	s.hub.Send(client, map[string]any{
		"type":       "activity_history",
		"activities": s.recentActivities(500),
	})
	s.hub.Send(client, inventoryStatusMessage(inventory))
}

func (s *Server) requestAgentRefresh(client *transport.ClientConn) {
	if client == nil {
		return
	}
	// Queue the deferred refresh before waking the poller. A publication
	// racing this handler can then drain it in the same ordered batch.
	s.refreshMu.Lock()
	s.refreshClients[client.ID()] = true
	s.refreshMu.Unlock()
	s.hub.SendBatchPrepared(client, func() []any {
		committed := s.committedInventorySnapshot()
		s.observeInventoryPublication("immediate")
		return committed.messages()
	})
	s.poller.Wake()
}

func (s *Server) sendRequestedAgentRefreshes() {
	s.refreshMu.Lock()
	clientIDs := make([]string, 0, len(s.refreshClients))
	for clientID := range s.refreshClients {
		clientIDs = append(clientIDs, clientID)
	}
	clear(s.refreshClients)
	s.refreshMu.Unlock()

	for _, clientID := range clientIDs {
		s.hub.SendBatchPreparedByID(clientID, func() []any {
			committed := s.committedInventorySnapshot()
			s.observeInventoryPublication("deferred")
			return committed.messages()
		})
	}
}
