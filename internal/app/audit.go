package app

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/0cv/herdr-mobile-relay/internal/audit"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

func serializedState(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func (s *Server) recordWriteAudit(
	client *transport.ClientConn,
	message map[string]any,
	result *coordinator.CommandResult,
) {
	if s.auditLog == nil {
		return
	}
	action := auditAction(message)
	if action == "" {
		return
	}
	requestID, _ := message["request_id"].(string)
	paneID, _ := message["pane_id"].(string)
	if paneID == "" {
		if target, ok := message["target"].(map[string]any); ok {
			paneID, _ = target["pane_id"].(string)
		}
	}
	clientID, _ := message["client_id"].(string)
	if clientID == "" {
		clientID = "connection:" + client.ID()
	}
	record := audit.Record{
		Stage:        "attempt",
		Action:       action,
		RequestID:    requestID,
		ClientID:     clientID,
		ConnectionID: client.ID(),
		PaneID:       paneID,
		Details:      auditWriteDetails(message),
	}
	if agent, ok := s.state.Agent(paneID); ok {
		record.Agent = agent.Agent
		record.Project = agent.Project
		record.Session = agent.Session
		record.Host = agent.Host
	}
	if result != nil {
		ok := result.OK
		record.Stage = "result"
		record.OK = &ok
		record.Phase = result.Phase
		record.Error = result.Error
		record.Details = nil
		if result.PaneID != "" {
			record.PaneID = result.PaneID
		}
	}
	if err := s.auditLog.Append(record); err != nil {
		s.logger.Warn(
			"remote write audit append failed",
			"stage", record.Stage,
			"action", record.Action,
			"request_id", record.RequestID,
			"error", err,
		)
	}
}

func auditAction(message map[string]any) string {
	action, _ := message["type"].(string)
	return action
}

func auditWriteDetails(message map[string]any) map[string]any {
	action := auditAction(message)
	if action == "" {
		return nil
	}
	if action == "set_pi_integration" {
		details := make(map[string]any)
		if enabled, ok := message["enabled"].(bool); ok {
			details["enabled"] = enabled
		}
		return details
	}
	// A secret answering a noecho prompt gets no payload digest and no keys: the
	// digest of a low-entropy secret is crackable offline and the keys spell the
	// secret out. Only its shape is auditable.
	if action == "send_secret" || action == "send_terminal_text" || action == "send_pi_prompt_unconfirmed" {
		details := make(map[string]any, 1)
		if text, ok := message["text"].(string); ok {
			details["text_bytes"] = len(text)
		}
		return details
	}
	details := make(map[string]any)
	if encoded, err := json.Marshal(message); err == nil {
		digest := sha256.Sum256(encoded)
		details["payload_sha256"] = fmt.Sprintf("%x", digest[:])
		details["payload_bytes"] = len(encoded)
	}
	stringLimits := map[string]int{
		"name": 256, "label": 256, "profile_id": 160, "workspace_id": 160,
		"before_workspace_id": 160, "cwd": 1024, "path": 1024, "branch": 512,
		"base": 512, "filename": 512, "mime": 128, "activity_label": 256,
	}
	for key, limit := range stringLimits {
		if value, ok := message[key].(string); ok && value != "" {
			details[key] = boundedAuditString(value, limit)
		}
	}
	for _, key := range []string{"text", "prompt", "choice", "data"} {
		if value, ok := message[key].(string); ok && value != "" {
			details[key+"_bytes"] = len(value)
		}
	}
	for _, key := range []string{"index", "insert_index", "total", "_server_sequence"} {
		if value, ok := auditInteger(message[key]); ok {
			details[key] = value
		}
	}
	if force, ok := message["force"].(bool); ok {
		details["force"] = force
	}
	if closeGroup, ok := message["close_group"].(bool); ok {
		details["close_group"] = closeGroup
	}
	if workspaceIDs := auditWorkspaceIDList(message["workspace_ids"]); len(workspaceIDs) > 0 {
		details["workspace_ids"] = workspaceIDs
	}
	if expectedWorkspaceIDs := auditWorkspaceIDList(message["expected_workspace_ids"]); len(expectedWorkspaceIDs) > 0 {
		details["expected_workspace_ids"] = expectedWorkspaceIDs
	}
	keys := make([]string, 0, 16)
	switch values := message["keys"].(type) {
	case []any:
		for _, value := range values {
			key, valid := value.(string)
			if !valid {
				continue
			}
			keys = append(keys, boundedAuditString(key, 64))
			if len(keys) == 32 {
				break
			}
		}
	}
	if len(keys) > 0 {
		details["keys"] = keys
	}
	indices := make([]int64, 0, 8)
	switch values := message["selected_indices"].(type) {
	case []any:
		for _, value := range values {
			if index, ok := auditInteger(value); ok {
				indices = append(indices, index)
			}
			if len(indices) == 128 {
				break
			}
		}
	}
	if len(indices) > 0 {
		details["selected_indices"] = indices
	}
	return details
}

func auditWorkspaceIDList(value any) []string {
	ids := make([]string, 0, 8)
	switch values := value.(type) {
	case []any:
		for _, item := range values {
			if id, ok := item.(string); ok && id != "" {
				ids = append(ids, boundedAuditString(id, 160))
			}
			if len(ids) == 32 {
				break
			}
		}
	}
	return ids
}

func boundedAuditString(value string, limit int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

func auditInteger(value any) (int64, bool) {
	switch number := value.(type) {
	case int:
		if number >= 0 {
			return int64(number), true
		}
	case int64:
		if number >= 0 {
			return number, true
		}
	case uint64:
		if number <= uint64(^uint64(0)>>1) {
			return int64(number), true
		}
	case float64:
		if number >= 0 && number <= 1<<53 && number == float64(int64(number)) {
			return int64(number), true
		}
	}
	return 0, false
}
