package app

import (
	"encoding/json"
	"errors"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
	"github.com/0cv/herdr-mobile-relay/internal/upload"
	"strings"
)

func (s *Server) validateUploadTarget(target protocol.TargetRef) error {
	if target.ServerSessionID != "primary" {
		return &upload.Error{Code: "upload_scope_mismatch"}
	}
	agent, exists := s.state.Agent(target.PaneID)
	if !exists || agent.TerminalID != target.TerminalID || agent.Generation != target.Generation ||
		target.AgentSessionID != agent.SessionID {
		return &upload.Error{Code: "upload_scope_mismatch"}
	}
	return nil
}
func (s *Server) expandPromptAttachmentReferences(action string, message map[string]any, target *protocol.TargetRef) error {
	switch action {
	case "prompt", "send_text", "submit_prompt", "text":
	default:
		return nil
	}
	for _, field := range []string{"text", "prompt"} {
		value, ok := message[field].(string)
		if !ok || !strings.Contains(value, "Attachment: ") {
			continue
		}
		lines := strings.Split(value, "\n")
		for index, line := range lines {
			ref, found := strings.CutPrefix(strings.TrimSpace(line), "Attachment: ")
			if !found || !validAttachmentReference(ref) {
				continue
			}
			if target == nil || s.uploadM == nil {
				return &upload.Error{Code: "attachment_reference_invalid"}
			}
			attachment, err := s.uploadM.Resolve(*target, ref)
			if err != nil {
				return &upload.Error{Code: "attachment_reference_invalid"}
			}
			lines[index] = "Attachment: " + attachment.Path
		}
		message[field] = strings.Join(lines, "\n")
	}
	return nil
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

func (s *Server) handleUploadBegin(client *transport.ClientConn, requestID string, message map[string]any) {
	var request upload.BeginRequest
	if err := decodeUploadRequest(message, &request); err != nil {
		s.sendUploadError(client, message, requestID, "upload_begin_result", err)
		return
	}
	if err := s.validateUploadTarget(request.Target); err != nil {
		s.sendUploadError(client, message, requestID, "upload_begin_result", err)
		return
	}
	if s.uploadM == nil {
		s.sendUploadError(client, message, requestID, "upload_begin_result", errors.New("upload_unavailable"))
		return
	}
	result, err := s.uploadM.Begin(request)
	if err != nil {
		s.sendUploadError(client, message, requestID, "upload_begin_result", err)
		return
	}
	s.sendUploadResult(client, message, requestID, "upload_begin_result", result)
}

func (s *Server) handleUploadChunk(client *transport.ClientConn, requestID string, message map[string]any) {
	var request upload.ChunkRequest
	if err := decodeUploadRequest(message, &request); err != nil {
		s.sendUploadError(client, message, requestID, "upload_chunk_result", err)
		return
	}
	if err := s.validateUploadTarget(request.Target); err != nil {
		if s.uploadM != nil {
			_ = s.uploadM.Cancel(request.Target, request.UploadID)
		}
		s.sendUploadError(client, message, requestID, "upload_chunk_result", err)
		return
	}
	if s.uploadM == nil {
		s.sendUploadError(client, message, requestID, "upload_chunk_result", errors.New("upload_unavailable"))
		return
	}
	result, err := s.uploadM.Chunk(request)
	if err != nil {
		s.sendUploadError(client, message, requestID, "upload_chunk_result", err)
		return
	}
	s.sendUploadResult(client, message, requestID, "upload_chunk_result", result)
}

func (s *Server) handleUploadFinish(client *transport.ClientConn, requestID string, message map[string]any) {
	var request upload.FinishRequest
	if err := decodeUploadRequest(message, &request); err != nil {
		s.sendUploadError(client, message, requestID, "upload_finish_result", err)
		return
	}
	if err := s.validateUploadTarget(request.Target); err != nil {
		if s.uploadM != nil {
			_ = s.uploadM.Cancel(request.Target, request.UploadID)
		}
		s.sendUploadError(client, message, requestID, "upload_finish_result", err)
		return
	}
	if s.uploadM == nil {
		s.sendUploadError(client, message, requestID, "upload_finish_result", errors.New("upload_unavailable"))
		return
	}
	result, err := s.uploadM.Finish(request)
	if err != nil {
		s.sendUploadError(client, message, requestID, "upload_finish_result", err)
		return
	}
	s.sendUploadResult(client, message, requestID, "upload_finish_result", result)
}

func (s *Server) handleUploadCancel(client *transport.ClientConn, requestID string, message map[string]any) {
	var request struct {
		Target   protocol.TargetRef `json:"target"`
		UploadID string             `json:"upload_id"`
	}
	if err := decodeUploadRequest(message, &request); err != nil {
		s.sendUploadError(client, message, requestID, "upload_cancel_result", err)
		return
	}
	if s.uploadM == nil {
		s.sendUploadError(client, message, requestID, "upload_cancel_result", errors.New("upload_unavailable"))
		return
	}
	if err := s.uploadM.Cancel(request.Target, request.UploadID); err != nil {
		s.sendUploadError(client, message, requestID, "upload_cancel_result", err)
		return
	}
	s.sendUploadResult(client, message, requestID, "upload_cancel_result", struct{}{})
}

func decodeUploadRequest(message map[string]any, destination any) error {
	encoded, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(encoded, destination); err != nil {
		return err
	}
	return nil
}

func (s *Server) sendUploadResult(client *transport.ClientConn, message map[string]any, requestID, messageType string, result any) {
	s.recordWriteAudit(client, message, &coordinator.CommandResult{
		RequestID: requestID,
		Action:    strings.TrimSuffix(messageType, "_result"),
		OK:        true,
		Phase:     "completed",
		Data:      result,
	})
	s.hub.Send(client, map[string]any{
		"type":       messageType,
		"request_id": requestID,
		"result":     result,
	})
}

func (s *Server) sendUploadError(client *transport.ClientConn, message map[string]any, requestID, messageType string, cause error) {
	code := "attachment_upload_failed"
	var args map[string]any
	var uploadError *upload.Error
	if errors.As(cause, &uploadError) {
		code = publicUploadErrorCode(uploadError.Code)
		args = uploadError.Args
	} else if cause != nil && cause.Error() == "upload_unavailable" {
		code = "attachment_upload_unavailable"
	}
	s.recordWriteAudit(client, message, &coordinator.CommandResult{
		RequestID: requestID,
		Action:    strings.TrimSuffix(messageType, "_result"),
		OK:        false,
		Phase:     "failed",
		Error:     code,
	})
	s.hub.Send(client, map[string]any{
		"type":       messageType,
		"request_id": requestID,
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
	case "upload_chunk_out_of_order", "upload_chunk_digest_mismatch", "upload_final_digest_mismatch",
		"upload_incomplete", "upload_scope_mismatch", "upload_session_not_found":
		return "attachment_upload_state_unknown"
	default:
		return "attachment_upload_failed"
	}
}
