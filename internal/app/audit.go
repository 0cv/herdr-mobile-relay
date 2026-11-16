package app

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/0cv/herdr-mobile-relay/internal/audit"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

func serializedState(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func (s *Server) recordWriteAudit(client *transport.ClientConn, inbound protocol.Inbound, result *coordinator.CommandResult) {
	action := inbound.Type
	if s.auditLog == nil || action == "" {
		return
	}
	paneID := inbound.PaneID
	if paneID == "" && inbound.Target != nil {
		paneID = inbound.Target.PaneID
	}
	clientID := inbound.ClientID
	if clientID == "" {
		clientID = "connection:" + client.ID()
	}
	record := audit.Record{
		Stage:        "attempt",
		Action:       action,
		RequestID:    inbound.RequestID,
		ClientID:     clientID,
		ConnectionID: client.ID(),
		PaneID:       paneID,
		Details:      auditWriteDetails(inbound),
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

func auditWriteDetails(inbound protocol.Inbound) map[string]any {
	action := inbound.Type
	if action == "" {
		return nil
	}
	if action == "set_pi_integration" {
		details := make(map[string]any)
		if inbound.Enabled != nil {
			details["enabled"] = *inbound.Enabled
		}
		return details
	}
	// A secret answering a noecho prompt gets no payload digest and no keys: the
	// digest of a low-entropy secret is crackable offline and the keys spell the
	// secret out. Only its shape is auditable.
	if action == "send_secret" || action == "send_terminal_text" || action == "send_pi_prompt_unconfirmed" {
		return map[string]any{"text_bytes": len(inbound.Text)}
	}
	details := make(map[string]any)
	if len(inbound.Raw) > 0 {
		digest := sha256.Sum256(inbound.Raw)
		details["payload_sha256"] = fmt.Sprintf("%x", digest[:])
		details["payload_bytes"] = len(inbound.Raw)
	}
	for key, value := range map[string]struct {
		text  string
		limit int
	}{
		"name": {inbound.Name, 256}, "label": {inbound.Label, 256}, "profile_id": {inbound.ProfileID, 160},
		"workspace_id": {inbound.WorkspaceID, 160}, "before_workspace_id": {inbound.BeforeWorkspaceID, 160},
		"cwd": {inbound.Cwd, 1024}, "path": {inbound.Path, 1024}, "branch": {inbound.Branch, 512},
		"base": {inbound.Base, 512}, "activity_label": {inbound.ActivityLabel, 256},
	} {
		if value.text != "" {
			details[key] = boundedAuditString(value.text, value.limit)
		}
	}
	for key, value := range map[string]string{
		"text": inbound.Text, "prompt": inbound.Prompt, "choice": inbound.Choice, "data": inbound.Data,
	} {
		if value != "" {
			details[key+"_bytes"] = len(value)
		}
	}
	for key, value := range map[string]*int{"index": inbound.Index, "insert_index": inbound.InsertIndex, "total": inbound.Total} {
		if value != nil && *value >= 0 {
			details[key] = int64(*value)
		}
	}
	if inbound.ReceiptSequence != 0 && inbound.ReceiptSequence <= math.MaxInt64 {
		details["_server_sequence"] = int64(inbound.ReceiptSequence)
	}
	if inbound.Force {
		details["force"] = true
	}
	if inbound.CloseGroup {
		details["close_group"] = true
	}
	if workspaceIDs := auditWorkspaceIDList(inbound.WorkspaceIDs); len(workspaceIDs) > 0 {
		details["workspace_ids"] = workspaceIDs
	}
	if expectedWorkspaceIDs := auditWorkspaceIDList(inbound.ExpectedWorkspaceIDs); len(expectedWorkspaceIDs) > 0 {
		details["expected_workspace_ids"] = expectedWorkspaceIDs
	}
	keys := make([]string, 0, min(len(inbound.Keys), 32))
	for _, key := range inbound.Keys {
		keys = append(keys, boundedAuditString(key, 64))
		if len(keys) == 32 {
			break
		}
	}
	if len(keys) > 0 {
		details["keys"] = keys
	}
	indices := make([]int64, 0, 8)
	for _, index := range inbound.SelectedIndices {
		if index >= 0 {
			indices = append(indices, int64(index))
		}
		if len(indices) == 128 {
			break
		}
	}
	if len(indices) > 0 {
		details["selected_indices"] = indices
	}
	return details
}

func auditWorkspaceIDList(values []string) []string {
	ids := make([]string, 0, 8)
	for _, id := range values {
		if id != "" {
			ids = append(ids, boundedAuditString(id, 160))
		}
		if len(ids) == 32 {
			break
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
