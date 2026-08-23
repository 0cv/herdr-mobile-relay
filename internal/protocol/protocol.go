package protocol

import (
	"encoding/json"
	"fmt"
)

const (
	Version                       = 2
	EncryptedWebSocketSubprotocol = "herdr-e2ee-v1"
	// HybridTransportCapability identifies the gateway + WebRTC hybrid
	// transport in release manifests (app_transports/relay_transports).
	HybridTransportCapability = "herdr-hybrid-v1"
)

var mutatingTypes = map[string]bool{
	"answer_question":     true,
	"clear_activities":    true,
	"navigate_question":   true,
	"respond":             true,
	"clarify_question":    true,
	"push_subscribe":      true,
	"push_unsubscribe":    true,
	"submit_prompt":       true,
	"prompt":              true,
	"send_keys":           true,
	"keys":                true,
	"send_text":           true,
	"text":                true,
	"send_secret":         true,
	"agent_start":         true,
	"agent_rename":        true,
	"tab_reorder":         true,
	"agent_stop":          true,
	"agent_clear":         true,
	"agent_restart":       true,
	"workspace_create":    true,
	"workspace_rename":    true,
	"workspace_reorder":   true,
	"workspace_close":     true,
	"worktree_create":     true,
	"worktree_open":       true,
	"worktree_remove":     true,
	"acknowledge_pane":    true,
	"upload_image":        true,
	"register_app_origin": true,
	"deploy_app_update":   true,
	"install_update":      true,
	"lease_pane_size":     true,
	"release_pane_size":   true,
}

type Inbound struct {
	Type              string          `json:"type"`
	Protocol          int             `json:"protocol"`
	RequestID         string          `json:"request_id,omitempty"`
	PaneID            string          `json:"pane_id,omitempty"`
	Text              string          `json:"text,omitempty"`
	Name              string          `json:"name,omitempty"`
	ProfileID         string          `json:"profile_id,omitempty"`
	Label             string          `json:"label,omitempty"`
	WorkspaceID       string          `json:"workspace_id,omitempty"`
	WorkspaceIDs      []string        `json:"workspace_ids,omitempty"`
	BeforeWorkspaceID string          `json:"before_workspace_id,omitempty"`
	Branch            string          `json:"branch,omitempty"`
	Base              string          `json:"base,omitempty"`
	Force             bool            `json:"force,omitempty"`
	Cwd               string          `json:"cwd,omitempty"`
	Prompt            string          `json:"prompt,omitempty"`
	EventID           string          `json:"event_id,omitempty"`
	InteractionID     string          `json:"interaction_id,omitempty"`
	InsertIndex       *int            `json:"insert_index,omitempty"`
	Index             *int            `json:"index,omitempty"`
	Total             *int            `json:"total,omitempty"`
	Keys              []string        `json:"keys,omitempty"`
	SelectedIndices   []int           `json:"selected_indices,omitempty"`
	OtherSelected     bool            `json:"other_selected,omitempty"`
	OtherText         string          `json:"other_text,omitempty"`
	Direction         string          `json:"direction,omitempty"`
	Lines             int             `json:"lines,omitempty"`
	Before            string          `json:"before,omitempty"`
	Limit             int             `json:"limit,omitempty"`
	Columns           int             `json:"columns,omitempty"`
	Rows              int             `json:"rows,omitempty"`
	Format            string          `json:"format,omitempty"`
	Path              string          `json:"path,omitempty"`
	Filename          string          `json:"filename,omitempty"`
	MIME              string          `json:"mime,omitempty"`
	Data              string          `json:"data,omitempty"`
	ClientID          string          `json:"client_id,omitempty"`
	ReplaceEndpoints  []string        `json:"replace_endpoints,omitempty"`
	NotifyFinished    bool            `json:"notify_finished,omitempty"`
	Endpoints         []string        `json:"endpoints,omitempty"`
	Origin            string          `json:"origin,omitempty"`
	ExpectedOrigin    string          `json:"expected_origin,omitempty"`
	ExpectedVersion   string          `json:"expected_version,omitempty"`
	ExpectedRevision  string          `json:"expected_revision,omitempty"`
	Subscription      json.RawMessage `json:"subscription,omitempty"`
}

func DecodeMap(raw map[string]any) (Inbound, error) {
	data, err := json.Marshal(raw)
	if err != nil {
		return Inbound{}, err
	}
	var message Inbound
	if err := json.Unmarshal(data, &message); err != nil {
		return Inbound{}, err
	}
	if action, _ := raw["action"].(string); action != "" {
		if message.Type == "" || message.Type == "command" {
			message.Type = action
		}
	}
	if message.Type == "" {
		return Inbound{}, fmt.Errorf("message type is required")
	}
	return message, nil
}

func RequiresProtocol(messageType string) bool {
	return mutatingTypes[messageType] && messageType != "install_update"
}

func Compatible(message Inbound) bool {
	return !RequiresProtocol(message.Type) || message.Protocol == Version
}

func IncompatibleResponse(message Inbound) map[string]any {
	version := any(message.Protocol)
	if message.Protocol == 0 {
		version = "invalid"
	}
	publicError := fmt.Sprintf("Incompatible app protocol v%v; relay requires v%d", version, Version)
	switch message.Type {
	case "upload_image":
		return map[string]any{
			"type":       "upload_result",
			"ok":         false,
			"error":      publicError,
			"path":       "",
			"pane_id":    message.PaneID,
			"request_id": message.RequestID,
		}
	case "push_subscribe":
		return map[string]any{"type": "push_subscribed", "ok": false, "error": publicError}
	case "push_unsubscribe":
		return map[string]any{"type": "push_unsubscribed", "ok": false, "error": publicError}
	default:
		return map[string]any{
			"type":       "command_result",
			"request_id": message.RequestID,
			"action":     message.Type,
			"ok":         false,
			"phase":      "failed",
			"error":      publicError,
		}
	}
}

func DecodeFailureResponse(raw map[string]any) map[string]any {
	messageType, _ := raw["type"].(string)
	if action, _ := raw["action"].(string); action != "" && (messageType == "" || messageType == "command") {
		messageType = action
	}
	requestID, _ := raw["request_id"].(string)
	paneID, _ := raw["pane_id"].(string)
	const publicError = "Invalid request"
	switch messageType {
	case "upload_image":
		return map[string]any{
			"type":       "upload_result",
			"ok":         false,
			"error":      publicError,
			"path":       "",
			"pane_id":    paneID,
			"request_id": requestID,
		}
	case "push_subscribe":
		return map[string]any{"type": "push_subscribed", "ok": false, "error": publicError}
	case "push_unsubscribe":
		return map[string]any{"type": "push_unsubscribed", "ok": false, "error": publicError}
	default:
		return map[string]any{
			"type":       "command_result",
			"request_id": requestID,
			"action":     messageType,
			"ok":         false,
			"phase":      "failed",
			"error":      publicError,
		}
	}
}

type PushConfig struct {
	Type           string   `json:"type"`
	VAPIDPublicKey string   `json:"vapid_public_key"`
	Host           string   `json:"host"`
	Protocol       int      `json:"protocol"`
	Version        string   `json:"version"`
	ReleaseVersion string   `json:"release_version"`
	Revision       string   `json:"revision"`
	Update         any      `json:"update"`
	AppDeploy      any      `json:"app_deploy"`
	Capabilities   []string `json:"capabilities"`
	Inventory      any      `json:"inventory"`
	AgentProfiles  any      `json:"agent_profiles"`
	// Hybrid advertises the gateway + direct WebRTC descriptor to an app that
	// connected over the legacy WSS URL, so the bridge window needs no QR
	// re-scan. Omitted entirely when the relay has no gateway configured.
	Hybrid map[string]any `json:"hybrid,omitempty"`
}

const AgentResponseCopyCapability = "agent_response_copy"

var Capabilities = []string{
	"attention_classification",
	"clear_activities",
	"directory_browser",
	"workspace_management",
	"worktree_management",
	"self_update",
	"structured_questions",
	"slash_commands",
	"conversation_history",
	"pane_size_lease",
	"pane_size_lease_rows",
	"workspace_inspection",
	"secret_input",
}
