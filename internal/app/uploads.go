package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
	"github.com/0cv/herdr-mobile-relay/internal/upload"
	"strings"
)

func (s *Server) validateUploadTarget(target protocol.TargetRef) error {
	agent, exists := s.state.Agent(target.PaneID)
	if !exists || !coordinator.TargetMatchesAgent(target, agent, agent.Generation) {
		return &upload.Error{Code: "upload_scope_mismatch"}
	}
	return nil
}
func (s *Server) expandPromptAttachmentReferences(action string, inbound protocol.Inbound) (protocol.Inbound, error) {
	if action != "submit_prompt" || !strings.Contains(inbound.Text, "Attachment: ") {
		return inbound, nil
	}
	lines := strings.Split(inbound.Text, "\n")
	for index, line := range lines {
		ref, found := strings.CutPrefix(strings.TrimSpace(line), "Attachment: ")
		if !found || !validAttachmentReference(ref) {
			continue
		}
		if inbound.Target == nil || s.uploadM == nil {
			return inbound, &upload.Error{Code: "attachment_reference_invalid"}
		}
		attachment, err := s.uploadM.Resolve(*inbound.Target, ref)
		if err != nil {
			return inbound, &upload.Error{Code: "attachment_reference_invalid"}
		}
		lines[index] = "Attachment: " + attachment.Path
	}
	inbound.Text = strings.Join(lines, "\n")
	return inbound, nil
}

func validAttachmentReference(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			character == '-' || character == '_' {
			continue
		}
		return false
	}
	return true
}

func (s *Server) handleUploadBegin(client *transport.ClientConn, inbound protocol.Inbound) {
	var request upload.BeginRequest
	if err := json.Unmarshal(inbound.Raw, &request); err != nil {
		s.sendUploadError(client, inbound, "upload_begin_result", err)
		return
	}
	if err := s.validateUploadTarget(request.Target); err != nil {
		s.sendUploadError(client, inbound, "upload_begin_result", err)
		return
	}
	if s.uploadM == nil {
		s.sendUploadError(client, inbound, "upload_begin_result", errors.New("upload_unavailable"))
		return
	}
	if identity, authenticated := client.Identity(); authenticated {
		request.Owner = identity.DeviceID
	} else {
		request.Owner = "connection:" + client.ID()
	}
	result, err := s.uploadM.Begin(request)
	if err != nil {
		s.sendUploadError(client, inbound, "upload_begin_result", err)
		return
	}
	s.sendUploadResult(client, inbound, "upload_begin_result", result)
}

func (s *Server) handleUploadChunk(client *transport.ClientConn, inbound protocol.Inbound) {
	var request upload.ChunkRequest
	if err := json.Unmarshal(inbound.Raw, &request); err != nil {
		s.sendUploadError(client, inbound, "upload_chunk_result", err)
		return
	}
	if err := s.validateUploadTarget(request.Target); err != nil {
		if s.uploadM != nil {
			_ = s.uploadM.Cancel(request.Target, request.UploadID)
		}
		s.sendUploadError(client, inbound, "upload_chunk_result", err)
		return
	}
	if s.uploadM == nil {
		s.sendUploadError(client, inbound, "upload_chunk_result", errors.New("upload_unavailable"))
		return
	}
	result, err := s.uploadM.Chunk(request)
	if err != nil {
		s.sendUploadError(client, inbound, "upload_chunk_result", err)
		return
	}
	s.sendUploadResult(client, inbound, "upload_chunk_result", result)
}

func (s *Server) handleUploadFinish(client *transport.ClientConn, inbound protocol.Inbound) {
	var request upload.FinishRequest
	if err := json.Unmarshal(inbound.Raw, &request); err != nil {
		s.sendUploadError(client, inbound, "upload_finish_result", err)
		return
	}
	if err := s.validateUploadTarget(request.Target); err != nil {
		if s.uploadM != nil {
			_ = s.uploadM.Cancel(request.Target, request.UploadID)
		}
		s.sendUploadError(client, inbound, "upload_finish_result", err)
		return
	}
	if s.uploadM == nil {
		s.sendUploadError(client, inbound, "upload_finish_result", errors.New("upload_unavailable"))
		return
	}
	result, err := s.uploadM.Finish(request)
	if err != nil {
		s.sendUploadError(client, inbound, "upload_finish_result", err)
		return
	}
	if s.dispatcher != nil {
		summary := fmt.Sprintf("Attached %d files", len(result.Attachments))
		if len(result.Attachments) == 1 {
			summary = "Attached " + result.Attachments[0].Name
		}
		s.dispatcher.RecordActivity("upload", "completed", summary, request.Target.PaneID, inbound.RequestID)
	}
	s.sendUploadResult(client, inbound, "upload_finish_result", result)
}

func (s *Server) handleUploadCancel(client *transport.ClientConn, inbound protocol.Inbound) {
	var request struct {
		Target   protocol.TargetRef `json:"target"`
		UploadID string             `json:"upload_id"`
	}
	if err := json.Unmarshal(inbound.Raw, &request); err != nil {
		s.sendUploadError(client, inbound, "upload_cancel_result", err)
		return
	}
	if s.uploadM == nil {
		s.sendUploadError(client, inbound, "upload_cancel_result", errors.New("upload_unavailable"))
		return
	}
	if err := s.uploadM.Cancel(request.Target, request.UploadID); err != nil {
		s.sendUploadError(client, inbound, "upload_cancel_result", err)
		return
	}
	s.sendUploadResult(client, inbound, "upload_cancel_result", struct{}{})
}

func (s *Server) sendUploadResult(client *transport.ClientConn, inbound protocol.Inbound, messageType string, result any) {
	action := strings.TrimSuffix(messageType, "_result")
	s.recordWriteAudit(client, inbound, &coordinator.CommandResult{
		RequestID: inbound.RequestID,
		Action:    action,
		OK:        true,
		Phase:     "completed",
		Data:      result,
	})
	s.hub.Send(client, map[string]any{
		"type":       messageType,
		"request_id": inbound.RequestID,
		"result":     result,
	})
}

func (s *Server) sendUploadError(client *transport.ClientConn, inbound protocol.Inbound, messageType string, cause error) {
	code := "attachment_upload_failed"
	var args map[string]any
	var uploadError *upload.Error
	if errors.As(cause, &uploadError) {
		code = publicUploadErrorCode(uploadError.Code)
		args = uploadError.Args
	} else if cause != nil && cause.Error() == "upload_unavailable" {
		code = "attachment_upload_unavailable"
	}
	action := strings.TrimSuffix(messageType, "_result")
	s.recordWriteAudit(client, inbound, &coordinator.CommandResult{
		RequestID: inbound.RequestID,
		Action:    action,
		OK:        false,
		Phase:     "failed",
		Error:     code,
	})
	s.hub.Send(client, map[string]any{
		"type":       messageType,
		"request_id": inbound.RequestID,
		"error":      protocol.NewApiError(code, args),
	})
}

func publicUploadErrorCode(code string) string {
	switch code {
	case "upload_batch_count_invalid":
		return "attachment_batch_limit"
	case "upload_batch_too_large":
		return "attachment_batch_too_large"
	case "upload_file_size_invalid", "upload_file_too_large":
		return "attachment_file_too_large"
	case "upload_name_invalid":
		return "attachment_invalid_name"
	case "upload_type_unsupported", "upload_extension_mismatch", "upload_content_type_mismatch":
		return "attachment_unknown_mime"
	case "upload_session_expired":
		return "attachment_upload_expired"
	case "upload_session_limit":
		return "attachment_upload_busy"
	case "upload_chunk_out_of_order", "upload_chunk_digest_mismatch", "upload_final_digest_mismatch",
		"upload_incomplete", "upload_scope_mismatch", "upload_session_not_found":
		return "attachment_upload_state_unknown"
	default:
		return "attachment_upload_failed"
	}
}
