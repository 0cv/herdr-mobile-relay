package app

import (
	"context"
	"encoding/base64"
	"errors"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/fsutil"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/setuphelper"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

func (s *Server) handleMessage(ctx context.Context, client *transport.ClientConn, msg map[string]any, admitted func()) {
	defer admitted()
	inbound, err := protocol.DecodeMap(msg)
	if err != nil {
		s.rejectMessage(client, admitted, protocol.DecodeFailureResponse(msg))
		return
	}
	scope, knownAction := protocol.ScopeFor(inbound)
	if !knownAction {
		s.rejectMessage(client, admitted, protocol.ErrorResponse(inbound.RequestID, protocol.NewApiError(
			protocol.ErrorUnknownAction,
			map[string]any{"operation": inbound.Type},
		)))
		return
	}
	if !protocol.Compatible(inbound) {
		s.rejectMessage(client, admitted, protocol.IncompatibleResponse(inbound))
		return
	}
	requestedSessionID := scope.ServerSessionID
	if scope.Target != nil {
		if requestedSessionID != "" && scope.Target.ServerSessionID != "" && requestedSessionID != scope.Target.ServerSessionID {
			s.rejectMessage(client, admitted, protocol.ErrorResponse(inbound.RequestID, protocol.NewApiError(
				protocol.ErrorInvalidRequest,
				map[string]any{"field": "server_session_id"},
			)))
			return
		}
		if requestedSessionID == "" {
			requestedSessionID = scope.Target.ServerSessionID
		}
	}
	if requestedSessionID != "" && requestedSessionID != protocol.PrimaryServerSessionID {
		s.rejectMessage(client, admitted, protocol.ErrorResponse(inbound.RequestID, protocol.NewApiError(
			protocol.ErrorInvalidRequest,
			map[string]any{"field": "server_session_id"},
		)))
		return
	}
	if authorizationErr := s.authorizeDeviceAction(client, scope.Action, inbound.DeviceID); authorizationErr != nil {
		s.rejectMessage(client, admitted, protocol.ErrorResponse(inbound.RequestID, *authorizationErr))
		return
	}
	identity, authenticated := client.Identity()
	if targetErr := validateExactPaneTarget(s.state, inbound, authenticated); targetErr != nil {
		s.rejectMessage(client, admitted, protocol.ErrorResponse(inbound.RequestID, *targetErr))
		return
	}
	action := scope.Action.Operation
	coordinated := scope.Action.Coordinated
	auditedWrite := scope.Action.Audited
	if auditedWrite {
		s.recordWriteAudit(client, msg, action, nil)
	}
	if !coordinated {
		admitted()
	}

	commandCtx := herdr.WithDispatchCheck(client.Context(), func() error {
		if err := client.Context().Err(); err != nil {
			return err
		}
		if s.authorizeDeviceAction(client, scope.Action, inbound.DeviceID) != nil {
			return errors.New("device is no longer authorized")
		}
		if validateExactPaneTarget(s.state, inbound, authenticated) != nil {
			return coordinator.ErrPaneReplaced
		}
		return nil
	})
	switch action {
	case "get_pi_integration", "set_pi_integration":
		s.handlePiIntegration(commandCtx, client, inbound, msg)
	case "get_pi_prompt_capability", "send_pi_prompt_unconfirmed":
		result := s.handlePiPrompt(commandCtx, client, inbound, msg, admitted)
		s.sendResult(client, msg, action, auditedWrite, result)
	case "check_update", "install_update", "deploy_app_update":
		s.handleUpdate(ctx, client, inbound)
	case "lease_pane_size", "release_pane_size":
		s.handlePaneSizeCommand(client, inbound)
	case "read_pane":
		s.handleReadPane(ctx, client, inbound)
	case "watch_pane":
		s.startPaneWatch(client, inbound)
	case "unwatch_pane":
		s.stopPaneWatch(client.ID(), inbound.PaneID)
	case "pane_applied":
		s.handlePaneApplied(client, inbound)
	case "get_conversation_history":
		s.handleConversationHistory(client, inbound)
	case "get_conversation_updates":
		s.handleConversationUpdates(client, inbound)
	case "unwatch_conversation":
		reply := s.reply(client, inbound.RequestID, action, inbound.PaneID)
		if inbound.SubscriptionID == "" {
			reply.fail("Conversation subscription is required")
			break
		}
		s.stopConversationFeed(client.ID(), inbound.SubscriptionID)
		reply.complete(nil)
	case "device_list":
		s.handleDeviceList(client, inbound, action, identity, authenticated)
	case "create_device_invitation":
		s.handleCreateDeviceInvitation(client, inbound, msg, action, identity, authenticated)
	case "rename_device":
		s.handleRenameDevice(client, inbound, msg, action)
	case "revoke_device":
		s.handleRevokeDevice(client, inbound, msg, action)
	case "reset_devices":
		s.handleResetDevices(client, inbound, msg, action, identity, authenticated)
	case "get_activity":
		s.handleGetActivity(client, inbound)
	case "clear_activities":
		s.dispatcher.HandleClearActivities(inbound.RequestID, func(result *coordinator.CommandResult) {
			s.hub.Send(client, coordinator.CommandResultMessage(result))
		})
	case "upload_begin":
		s.handleUploadBegin(client, inbound.RequestID, msg)
	case "upload_chunk":
		s.handleUploadChunk(client, inbound.RequestID, msg)
	case "upload_finish":
		s.handleUploadFinish(client, inbound.RequestID, msg)
	case "upload_cancel":
		s.handleUploadCancel(client, inbound.RequestID, msg)
	case "workspace_create", "workspace_rename", "workspace_reorder", "workspace_close",
		"worktree_list", "worktree_create", "worktree_open", "worktree_remove":
		s.handleTopology(commandCtx, client, inbound, msg, action, auditedWrite, admitted)
	case "list_directories":
		listing := fsutil.ListDirectories(inbound.Path, s.home)
		s.reply(client, inbound.RequestID, "list_directories", "").complete(listing)
	case "list_slash_commands":
		s.handleListSlashCommands(client, inbound)
	case "workspace_tree", "workspace_file", "workspace_git_status", "workspace_git_diff":
		s.handleWorkspaceInspect(client, inbound)
	case "copy_agent_response":
		s.copyAgentResponse(client, inbound.RequestID, inbound.PaneID)
	case "cancel_speech":
		s.cancelSpeech(client.ID(), inbound.SpeechRequestID)
	case "speak_text":
		s.speakText(client, inbound.RequestID, inbound.SpeechRequestID, inbound.Text, inbound.Language)
	case "speech_voices_list":
		s.reply(client, inbound.RequestID, action, "").complete(s.speechVoicePayload(nil))
	case "speech_voice_install", "speech_voice_remove":
		s.changeSpeechVoice(client, inbound.RequestID, action, inbound.Language)
	case "qr_code":
		s.handleQRCode(client, inbound, action)
	case "push_policy_get":
		s.handlePushPolicyGet(client, identity)
	case "push_policy_set":
		s.handlePushPolicySet(client, inbound, identity)
	case "push_snooze":
		s.handlePushSnooze(client, inbound, identity)
	case "push_viewed_pane":
		s.handlePushViewedPane(client, inbound, identity)
	case "push_open_ref":
		s.handlePushOpenRef(client, inbound, action, identity)
	case "push_test_device":
		s.handlePushTestDevice(ctx, client, identity)
	case "push_subscribe":
		s.handlePushSubscribe(client, inbound, identity)
	case "push_unsubscribe":
		s.handlePushUnsubscribe(client, inbound, identity)
	case "register_app_origin":
		if err := s.storePhoneAppOrigin(inbound.Origin); err != nil {
			s.recordSafeError("phone app origin was not stored", err)
			s.logger.Warn("phone app origin was not stored", "error", err)
		}
	case "refresh_agents":
		s.requestAgentRefresh(client)
	case "webrtc_offer", "webrtc_ice", "webrtc_close":
		s.handleWebRTCSignal(commandCtx, client, inbound)
	default:
		s.handleDispatched(commandCtx, client, inbound, msg, action, coordinated, auditedWrite, admitted)
	}
}

func (s *Server) handleDisconnect(client *transport.ClientConn) {
	s.stopConversationFeed(client.ID(), "")
	s.stopPaneWatch(client.ID(), "")
	if identity, authenticated := client.Identity(); authenticated {
		s.pushM.SetViewedPane(identity.DeviceID, nil)
	}
	if s.hybrid != nil {
		s.hybrid.forgetClient(client.ID())
	}
	// A departing phone gives its panes back at once, without the
	// navigation grace. Restores that cannot finish now continue in the
	// manager's bounded cleanup.
	releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.paneSizeM.ReleaseClient(releaseCtx, client.ID())
}

func (s *Server) rejectMessage(client *transport.ClientConn, admitted func(), response map[string]any) {
	admitted()
	s.hub.Send(client, response)
}

func (s *Server) sendResult(client *transport.ClientConn, msg map[string]any, action string, auditedWrite bool, result *coordinator.CommandResult) {
	if auditedWrite {
		s.recordWriteAudit(client, msg, action, result)
	}
	s.hub.Send(client, coordinator.CommandResultMessage(result))
}

func (s *Server) handleGetActivity(client *transport.ClientConn, inbound protocol.Inbound) {
	limit := inbound.Limit
	if limit < 1 || limit > 500 {
		limit = 500
	}
	s.hub.Send(client, map[string]any{
		"type":       "activity_history",
		"activities": s.recentActivities(limit),
	})
}

func (s *Server) handleQRCode(client *transport.ClientConn, inbound protocol.Inbound, action string) {
	reply := s.reply(client, inbound.RequestID, action, "")
	size, packed, qrErr := setuphelper.PackedQR(inbound.Text)
	if qrErr != nil {
		s.logger.Warn("qr encoding failed", "error", qrErr)
		reply.fail("This computer could not encode that QR code")
		return
	}
	reply.complete(map[string]any{
		"size":    size,
		"modules": base64.StdEncoding.EncodeToString(packed),
	})
}

func (s *Server) handleDispatched(commandCtx context.Context, client *transport.ClientConn, inbound protocol.Inbound, msg map[string]any, action string, coordinated bool, auditedWrite bool, admitted func()) {
	inbound, err := s.expandPromptAttachmentReferences(action, inbound)
	if err != nil {
		result := &coordinator.CommandResult{
			RequestID: inbound.RequestID,
			Action:    action,
			Phase:     "failed",
			Error:     "One or more attachments are no longer available for this agent",
			PaneID:    inbound.PaneID,
		}
		s.sendResult(client, msg, action, auditedWrite, result)
		return
	}
	var result *coordinator.CommandResult
	if coordinated {
		result = s.dispatcher.HandleAdmitted(commandCtx, inbound, admitted)
	} else {
		result = s.dispatcher.Handle(commandCtx, inbound)
	}
	s.sendResult(client, msg, action, auditedWrite, result)
}
