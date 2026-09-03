package protocol

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	Version                       = 3
	EncryptedWebSocketSubprotocol = "herdr-e2ee-v2"
	HybridTransportCapability     = "herdr-hybrid-v2"
)

type TargetRef struct {
	ServerSessionID string `json:"server_session_id"`
	PaneID          string `json:"pane_id"`
	TerminalID      string `json:"terminal_id"`
	Generation      int64  `json:"generation"`
	AgentSessionID  string `json:"agent_session_id"`
}
type ActionReceiptPhase string

const (
	ReceiptPrepared             ActionReceiptPhase = "prepared"
	ReceiptFailedBeforeDispatch ActionReceiptPhase = "failed_before_dispatch"
	ReceiptAwaitingEvidence     ActionReceiptPhase = "awaiting_evidence"
	ReceiptConfirmed            ActionReceiptPhase = "confirmed"
	ReceiptDispatchedUnknown    ActionReceiptPhase = "dispatched_unknown"
)

type ApiError struct {
	Code string         `json:"code"`
	Args map[string]any `json:"args,omitempty"`
}

const (
	ErrorInvalidRequest       = "invalid_request"
	ErrorUnknownAction        = "unknown_action"
	ErrorIncompatibleProtocol = "incompatible_protocol"
	ErrorReaderDenied         = "reader_denied"
)

type ActionReceipt struct {
	ActionID string             `json:"action_id"`
	Phase    ActionReceiptPhase `json:"phase"`
	Error    *ApiError          `json:"error,omitempty"`
}

type DeviceRole string

const (
	RoleReader     DeviceRole = "reader"
	RoleController DeviceRole = "controller"
	RoleBootstrap  DeviceRole = "bootstrap"
)

type DeviceContext struct {
	DeviceID          string     `json:"device_id"`
	CredentialID      string     `json:"credential_id"`
	Role              DeviceRole `json:"role"`
	Locale            string     `json:"locale"`
	CredentialVersion int64      `json:"credential_version"`
}

type OpaquePage[T any] struct {
	Items       []T    `json:"items"`
	NextCursor  string `json:"next_cursor,omitempty"`
	Truncated   bool   `json:"truncated"`
	GeneratedAt int64  `json:"generated_at"`
}

type ActionClass string

const (
	ActionReadOnly ActionClass = "read_only"
	ActionMutating ActionClass = "mutating"
)

type ActionMetadata struct {
	Operation        string
	Class            ActionClass
	RequiresProtocol bool
	Coordinated      bool
	Audited          bool
}

func readAction(operation string) ActionMetadata {
	return ActionMetadata{Operation: operation, Class: ActionReadOnly}
}

func mutateAction(operation string, coordinated, audited bool) ActionMetadata {
	return ActionMetadata{
		Operation:        operation,
		Class:            ActionMutating,
		RequiresProtocol: operation != "install_update",
		Coordinated:      coordinated,
		Audited:          audited,
	}
}

var actionCatalog = map[string]ActionMetadata{
	"acknowledge_pane":         mutateAction("acknowledge_pane", true, false),
	"agent_clear":              mutateAction("agent_clear", true, true),
	"agent_rename":             mutateAction("agent_rename", true, true),
	"agent_restart":            mutateAction("agent_restart", true, true),
	"agent_start":              mutateAction("agent_start", true, true),
	"agent_stop":               mutateAction("agent_stop", true, true),
	"answer_question":          mutateAction("answer_question", true, true),
	"cancel_speech":            readAction("cancel_speech"),
	"check_update":             readAction("check_update"),
	"clarify_question":         mutateAction("clarify_question", true, true),
	"clear_activities":         mutateAction("clear_activities", false, false),
	"copy_agent_response":      mutateAction("copy_agent_response", false, false),
	"deploy_app_update":        mutateAction("deploy_app_update", false, false),
	"create_device_invitation": mutateAction("create_device_invitation", false, true),
	"device_list":              readAction("device_list"),
	"get_activity":             readAction("get_activity"),
	"get_conversation_history": readAction("get_conversation_history"),
	"install_update":           mutateAction("install_update", false, false),
	"lease_pane_size":          mutateAction("lease_pane_size", true, false),
	"list_directories":         readAction("list_directories"),
	"list_slash_commands":      readAction("list_slash_commands"),
	"navigate_question":        mutateAction("navigate_question", true, true),
	"pane_applied":             readAction("pane_applied"),
	"push_open_ref":            readAction("push_open_ref"),
	"push_policy_get":          readAction("push_policy_get"),
	"push_policy_set":          mutateAction("push_policy_set", false, false),
	"push_snooze":              mutateAction("push_snooze", false, false),
	"push_subscribe":           mutateAction("push_subscribe", false, false),
	"push_test_device":         mutateAction("push_test_device", false, true),
	"push_unsubscribe":         mutateAction("push_unsubscribe", false, false),
	"push_viewed_pane":         mutateAction("push_viewed_pane", false, false),
	"qr_code":                  readAction("qr_code"),
	"read_pane":                readAction("read_pane"),
	"refresh_agents":           readAction("refresh_agents"),
	"register_app_origin":      mutateAction("register_app_origin", false, false),
	"release_pane_size":        mutateAction("release_pane_size", true, false),
	"rename_device":            mutateAction("rename_device", false, true),
	"respond":                  mutateAction("respond", true, true),
	"send_keys":                mutateAction("send_keys", true, true),
	"send_input":               mutateAction("send_input", true, true),
	"send_secret":              mutateAction("send_secret", true, true),
	"reset_devices":            mutateAction("reset_devices", false, true),
	"revoke_device":            mutateAction("revoke_device", false, true),
	"send_text":                mutateAction("send_text", true, true),
	"speak_text":               readAction("speak_text"),
	"speech_voice_install":     mutateAction("speech_voice_install", false, true),
	"speech_voice_remove":      mutateAction("speech_voice_remove", false, true),
	"speech_voices_list":       readAction("speech_voices_list"),
	"submit_prompt":            mutateAction("submit_prompt", true, true),
	"tab_reorder":              mutateAction("tab_reorder", true, true),
	"unwatch_pane":             readAction("unwatch_pane"),
	"upload_begin":             mutateAction("upload_begin", false, true),
	"upload_cancel":            mutateAction("upload_cancel", false, true),
	"upload_chunk":             mutateAction("upload_chunk", false, true),
	"upload_finish":            mutateAction("upload_finish", false, true),
	"watch_pane":               readAction("watch_pane"),
	"webrtc_close":             readAction("webrtc_close"),
	"webrtc_ice":               readAction("webrtc_ice"),
	"webrtc_offer":             readAction("webrtc_offer"),
	"workspace_close":          mutateAction("workspace_close", true, true),
	"workspace_create":         mutateAction("workspace_create", true, true),
	"workspace_file":           readAction("workspace_file"),
	"workspace_git_diff":       readAction("workspace_git_diff"),
	"workspace_git_status":     readAction("workspace_git_status"),
	"workspace_rename":         mutateAction("workspace_rename", true, true),
	"workspace_reorder":        mutateAction("workspace_reorder", true, true),
	"workspace_tree":           readAction("workspace_tree"),
	"worktree_create":          mutateAction("worktree_create", true, true),
	"worktree_list":            readAction("worktree_list"),
	"worktree_open":            mutateAction("worktree_open", true, true),
	"worktree_remove":          mutateAction("worktree_remove", true, true),
}

type Inbound struct {
	Type                string          `json:"type"`
	Protocol            int             `json:"protocol"`
	RequestID           string          `json:"request_id,omitempty"`
	Target              *TargetRef      `json:"target,omitempty"`
	ActionID            string          `json:"action_id,omitempty"`
	ServerSessionID     string          `json:"server_session_id,omitempty"`
	SessionID           string          `json:"session_id,omitempty"`
	PaneID              string          `json:"pane_id,omitempty"`
	Text                string          `json:"text,omitempty"`
	Name                string          `json:"name,omitempty"`
	DeviceID            string          `json:"device_id,omitempty"`
	Role                string          `json:"role,omitempty"`
	Locale              string          `json:"locale,omitempty"`
	ProfileID           string          `json:"profile_id,omitempty"`
	Label               string          `json:"label,omitempty"`
	WorkspaceID         string          `json:"workspace_id,omitempty"`
	WorkspaceIDs        []string        `json:"workspace_ids,omitempty"`
	BeforeWorkspaceID   string          `json:"before_workspace_id,omitempty"`
	Branch              string          `json:"branch,omitempty"`
	Base                string          `json:"base,omitempty"`
	Force               bool            `json:"force,omitempty"`
	Cwd                 string          `json:"cwd,omitempty"`
	Prompt              string          `json:"prompt,omitempty"`
	EventID             string          `json:"event_id,omitempty"`
	ApprovalFingerprint string          `json:"approval_fingerprint,omitempty"`
	Choice              string          `json:"choice,omitempty"`
	InteractionID       string          `json:"interaction_id,omitempty"`
	InsertIndex         *int            `json:"insert_index,omitempty"`
	Index               *int            `json:"index,omitempty"`
	Total               *int            `json:"total,omitempty"`
	Keys                []string        `json:"keys,omitempty"`
	SelectedIndices     []int           `json:"selected_indices,omitempty"`
	OtherSelected       bool            `json:"other_selected,omitempty"`
	OtherText           string          `json:"other_text,omitempty"`
	Direction           string          `json:"direction,omitempty"`
	Lines               int             `json:"lines,omitempty"`
	Before              string          `json:"before,omitempty"`
	Limit               int             `json:"limit,omitempty"`
	Columns             int             `json:"columns,omitempty"`
	Rows                int             `json:"rows,omitempty"`
	Format              string          `json:"format,omitempty"`
	Path                string          `json:"path,omitempty"`
	Filename            string          `json:"filename,omitempty"`
	MIME                string          `json:"mime,omitempty"`
	Data                string          `json:"data,omitempty"`
	ClientID            string          `json:"client_id,omitempty"`
	ReplaceEndpoints    []string        `json:"replace_endpoints,omitempty"`
	NotifyFinished      bool            `json:"notify_finished,omitempty"`
	Endpoints           []string        `json:"endpoints,omitempty"`
	Origin              string          `json:"origin,omitempty"`
	ExpectedOrigin      string          `json:"expected_origin,omitempty"`
	ExpectedVersion     string          `json:"expected_version,omitempty"`
	ExpectedRevision    string          `json:"expected_revision,omitempty"`
	Subscription        json.RawMessage `json:"subscription,omitempty"`
	Policy              json.RawMessage `json:"policy,omitempty"`
	EventRef            string          `json:"event_ref,omitempty"`
	SnoozeUntil         string          `json:"snooze_until,omitempty"`
	Snoozed             bool            `json:"snoozed,omitempty"`
	Visible             bool            `json:"visible,omitempty"`
	Unlocked            bool            `json:"unlocked,omitempty"`
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

type RequestScope struct {
	Action          ActionMetadata
	Target          *TargetRef
	ActionID        string
	ServerSessionID string
	SessionID       string
}

func ScopeFor(message Inbound) (RequestScope, bool) {
	metadata, known := ClassifyAction(message.Type)
	if !known {
		return RequestScope{}, false
	}
	return RequestScope{
		Action:          metadata,
		Target:          message.Target,
		ActionID:        message.ActionID,
		ServerSessionID: message.ServerSessionID,
		SessionID:       message.SessionID,
	}, true
}

func ClassifyAction(operation string) (ActionMetadata, bool) {
	metadata, known := actionCatalog[operation]
	return metadata, known
}

func RequiresProtocol(messageType string) bool {
	metadata, known := ClassifyAction(messageType)
	return !known || metadata.RequiresProtocol
}

func Compatible(message Inbound) bool {
	return !RequiresProtocol(message.Type) || message.Protocol == Version
}

const (
	maxErrorArgs      = 8
	maxErrorArgKey    = 32
	maxErrorArgString = 256
	maxErrorArgsJSON  = 1024
)

func NewApiError(code string, args map[string]any) ApiError {
	result := ApiError{Code: boundedUTF8(strings.ToLower(strings.TrimSpace(code)), 64)}
	if len(args) == 0 {
		return result
	}
	keys := make([]string, 0, len(args))
	for key := range args {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result.Args = make(map[string]any, min(len(keys), maxErrorArgs))
	for _, key := range keys {
		if len(result.Args) == maxErrorArgs {
			break
		}
		normalizedKey := boundedUTF8(strings.ToLower(strings.TrimSpace(key)), maxErrorArgKey)
		if normalizedKey == "" {
			continue
		}
		value, valid := boundedErrorArg(args[key])
		if !valid {
			continue
		}
		result.Args[normalizedKey] = value
		if encoded, err := json.Marshal(result.Args); err != nil || len(encoded) > maxErrorArgsJSON {
			delete(result.Args, normalizedKey)
			break
		}
	}
	if len(result.Args) == 0 {
		result.Args = nil
	}
	return result
}

func boundedErrorArg(value any) (any, bool) {
	switch typed := value.(type) {
	case string:
		return boundedUTF8(typed, maxErrorArgString), true
	case bool:
		return typed, true
	case int:
		return typed, true
	case int8:
		return typed, true
	case int16:
		return typed, true
	case int32:
		return typed, true
	case int64:
		if typed >= -(1<<53)+1 && typed <= (1<<53)-1 {
			return typed, true
		}
	case uint:
		if uint64(typed) <= (1<<53)-1 {
			return typed, true
		}
	case uint8:
		return typed, true
	case uint16:
		return typed, true
	case uint32:
		return typed, true
	case uint64:
		if typed <= (1<<53)-1 {
			return typed, true
		}
	case float64:
		if !math.IsNaN(typed) && !math.IsInf(typed, 0) && typed == math.Trunc(typed) &&
			typed >= -(1<<53)+1 && typed <= (1<<53)-1 {
			return typed, true
		}
	}
	return nil, false
}

func boundedUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func ErrorResponse(requestID string, apiError ApiError) map[string]any {
	response := map[string]any{
		"type":  "error",
		"error": apiError,
	}
	if requestID != "" {
		response["request_id"] = requestID
	}
	return response
}

func ActionReceiptResponse(requestID string, receipt ActionReceipt) map[string]any {
	response := map[string]any{
		"type":    "action_receipt",
		"receipt": receipt,
	}
	if requestID != "" {
		response["request_id"] = requestID
	}
	return response
}

func IncompatibleResponse(message Inbound) map[string]any {
	received := fmt.Sprint(message.Protocol)
	if message.Protocol == 0 {
		received = "invalid"
	}
	apiError := NewApiError(ErrorIncompatibleProtocol, map[string]any{
		"received": received,
		"required": fmt.Sprint(Version),
	})
	return ActionReceiptResponse(message.RequestID, ActionReceipt{
		ActionID: message.ActionID,
		Phase:    ReceiptFailedBeforeDispatch,
		Error:    &apiError,
	})
}

func DecodeFailureResponse(raw map[string]any) map[string]any {
	requestID, _ := raw["request_id"].(string)
	return ErrorResponse(requestID, NewApiError(ErrorInvalidRequest, nil))
}

type PushConfig struct {
	Type           string `json:"type"`
	VAPIDPublicKey string `json:"vapid_public_key"`
	Host           string `json:"host"`
	// Home lets the phone print a checkout as "~/code/app" instead of the
	// computer's absolute path, which rarely fits a phone row.
	Home           string   `json:"home"`
	Protocol       int      `json:"protocol"`
	Version        string   `json:"version"`
	ReleaseVersion string   `json:"release_version"`
	Revision       string   `json:"revision"`
	Update         any      `json:"update"`
	AppDeploy      any      `json:"app_deploy"`
	Capabilities   []string `json:"capabilities"`
	// SpeechLanguages lists the languages this host has a voice for, so the
	// phone offers only what the relay can actually read aloud.
	SpeechLanguages []string `json:"speech_languages,omitempty"`
	Inventory       any      `json:"inventory"`
	AgentProfiles   any      `json:"agent_profiles"`
	// Hybrid advertises the gateway + direct WebRTC descriptor to an app that
	// connected over the legacy WSS URL, so the bridge window needs no QR
	// re-scan. Omitted entirely when the relay has no gateway configured.
	Hybrid map[string]any `json:"hybrid,omitempty"`
}

const AgentResponseCopyCapability = "agent_response_copy"

const SpeechSynthesisCapability = "speech_synthesis"
const SpeechVoiceManagementCapability = "speech_voice_management"

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
	"semantic_input",
	"secret_input",
	"invitation_qr",
}
