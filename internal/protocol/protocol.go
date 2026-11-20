package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

const (
	Version                       = 3
	EncryptedWebSocketSubprotocol = "herdr-e2ee-v2"
	HybridTransportCapability     = "herdr-hybrid-v2"
	PrimaryServerSessionID        = "primary"
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
	ReceiptFailedBeforeDispatch ActionReceiptPhase = "failed_before_dispatch"
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
)

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

func readAction() ActionMetadata {
	return ActionMetadata{Class: ActionReadOnly}
}

func mutateAction(coordinated, audited bool) ActionMetadata {
	return ActionMetadata{
		Class:            ActionMutating,
		RequiresProtocol: true,
		Coordinated:      coordinated,
		Audited:          audited,
	}
}

var actionCatalog = map[string]ActionMetadata{
	"acknowledge_pane":           mutateAction(true, false),
	"agent_clear":                mutateAction(true, true),
	"agent_rename":               mutateAction(true, true),
	"agent_start":                mutateAction(true, true),
	"agent_stop":                 mutateAction(true, true),
	"answer_question":            mutateAction(true, true),
	"cancel_speech":              readAction(),
	"check_update":               readAction(),
	"clarify_question":           mutateAction(true, true),
	"clear_activities":           mutateAction(false, false),
	"copy_agent_response":        mutateAction(false, false),
	"deploy_app_update":          mutateAction(false, false),
	"create_device_invitation":   mutateAction(false, true),
	"device_list":                readAction(),
	"get_pi_integration":         readAction(),
	"set_pi_integration":         mutateAction(false, true),
	"get_activity":               readAction(),
	"get_conversation_history":   readAction(),
	"get_conversation_updates":   readAction(),
	"unwatch_conversation":       readAction(),
	"install_update":             {Class: ActionMutating},
	"lease_pane_size":            mutateAction(true, false),
	"list_directories":           readAction(),
	"list_slash_commands":        readAction(),
	"navigate_question":          mutateAction(true, true),
	"pane_applied":               readAction(),
	"push_open_ref":              readAction(),
	"push_policy_get":            readAction(),
	"push_policy_set":            mutateAction(false, false),
	"push_snooze":                mutateAction(false, false),
	"push_subscribe":             mutateAction(false, false),
	"push_test_device":           mutateAction(false, true),
	"push_unsubscribe":           mutateAction(false, false),
	"push_viewed_pane":           mutateAction(false, false),
	"qr_code":                    readAction(),
	"read_pane":                  readAction(),
	"refresh_agents":             readAction(),
	"register_app_origin":        mutateAction(false, false),
	"release_pane_size":          mutateAction(true, false),
	"rename_device":              mutateAction(false, true),
	"respond":                    mutateAction(true, true),
	"send_keys":                  mutateAction(true, true),
	"send_input":                 mutateAction(true, true),
	"send_terminal_text":         mutateAction(true, true),
	"send_filter_text":           mutateAction(true, true),
	"send_secret":                mutateAction(true, true),
	"reset_devices":              mutateAction(false, true),
	"revoke_device":              mutateAction(false, true),
	"speak_text":                 readAction(),
	"speech_voice_install":       mutateAction(false, true),
	"speech_voice_remove":        mutateAction(false, true),
	"speech_voices_list":         readAction(),
	"submit_prompt":              mutateAction(true, true),
	"get_pi_prompt_capability":   readAction(),
	"send_pi_prompt_unconfirmed": mutateAction(true, true),
	"tab_reorder":                mutateAction(true, true),
	"unwatch_pane":               readAction(),
	"upload_begin":               mutateAction(false, true),
	"upload_cancel":              mutateAction(false, true),
	"upload_chunk":               mutateAction(false, true),
	"upload_finish":              mutateAction(false, true),
	"watch_pane":                 readAction(),
	"webrtc_close":               readAction(),
	"webrtc_ice":                 readAction(),
	"webrtc_offer":               readAction(),
	"workspace_close":            mutateAction(true, true),
	"workspace_create":           mutateAction(true, true),
	"workspace_file":             readAction(),
	"workspace_git_diff":         readAction(),
	"workspace_git_status":       readAction(),
	"workspace_rename":           mutateAction(true, true),
	"workspace_reorder":          mutateAction(true, true),
	"workspace_tree":             readAction(),
	"worktree_create":            mutateAction(true, true),
	"worktree_list":              readAction(),
	"worktree_open":              mutateAction(true, true),
	"worktree_remove":            mutateAction(true, true),
}

type Inbound struct {
	Type                 string          `json:"type"`
	Protocol             int             `json:"protocol"`
	RequestID            string          `json:"request_id,omitempty"`
	Target               *TargetRef      `json:"target,omitempty"`
	ActionID             string          `json:"action_id,omitempty"`
	ServerSessionID      string          `json:"server_session_id,omitempty"`
	SessionID            string          `json:"session_id,omitempty"`
	PaneID               string          `json:"pane_id,omitempty"`
	Text                 string          `json:"text,omitempty"`
	PiPromptLease        json.RawMessage `json:"pi_prompt_lease,omitempty"`
	Name                 string          `json:"name,omitempty"`
	DeviceID             string          `json:"device_id,omitempty"`
	Role                 string          `json:"role,omitempty"`
	Locale               string          `json:"locale,omitempty"`
	ProfileID            string          `json:"profile_id,omitempty"`
	Label                string          `json:"label,omitempty"`
	WorkspaceID          string          `json:"workspace_id,omitempty"`
	WorkspaceIDs         []string        `json:"workspace_ids,omitempty"`
	ExpectedWorkspaceIDs []string        `json:"expected_workspace_ids,omitempty"`
	CloseGroup           bool            `json:"close_group,omitempty"`
	BeforeWorkspaceID    string          `json:"before_workspace_id,omitempty"`
	Branch               string          `json:"branch,omitempty"`
	Base                 string          `json:"base,omitempty"`
	Force                bool            `json:"force,omitempty"`
	Cwd                  string          `json:"cwd,omitempty"`
	Prompt               string          `json:"prompt,omitempty"`
	EventID              string          `json:"event_id,omitempty"`
	ApprovalFingerprint  string          `json:"approval_fingerprint,omitempty"`
	Choice               string          `json:"choice,omitempty"`
	InteractionID        string          `json:"interaction_id,omitempty"`
	InsertIndex          *int            `json:"insert_index,omitempty"`
	Index                *int            `json:"index,omitempty"`
	Total                *int            `json:"total,omitempty"`
	Keys                 []string        `json:"keys,omitempty"`
	SelectedIndices      []int           `json:"selected_indices,omitempty"`
	OtherSelected        bool            `json:"other_selected,omitempty"`
	OtherText            string          `json:"other_text,omitempty"`
	Direction            string          `json:"direction,omitempty"`
	Lines                *int            `json:"lines,omitempty"`
	Cursor               string          `json:"cursor,omitempty"`
	SubscriptionID       string          `json:"subscription_id,omitempty"`
	Observe              bool            `json:"observe,omitempty"`
	After                uint64          `json:"after,omitempty"`
	SourceRevision       string          `json:"source_revision,omitempty"`
	Retry                bool            `json:"retry,omitempty"`
	Limit                int             `json:"limit,omitempty"`
	Columns              int             `json:"columns,omitempty"`
	Rows                 int             `json:"rows,omitempty"`
	OptIn                bool            `json:"opt_in,omitempty"`
	Immediate            bool            `json:"immediate,omitempty"`
	Format               string          `json:"format,omitempty"`
	Path                 string          `json:"path,omitempty"`
	Data                 string          `json:"data,omitempty"`
	ClientID             string          `json:"client_id,omitempty"`
	ReplaceEndpoints     []string        `json:"replace_endpoints,omitempty"`
	Endpoints            []string        `json:"endpoints,omitempty"`
	Origin               string          `json:"origin,omitempty"`
	ExpectedOrigin       string          `json:"expected_origin,omitempty"`
	ExpectedVersion      string          `json:"expected_version,omitempty"`
	ExpectedRevision     string          `json:"expected_revision,omitempty"`
	Subscription         json.RawMessage `json:"subscription,omitempty"`
	Policy               json.RawMessage `json:"policy,omitempty"`
	EventRef             string          `json:"event_ref,omitempty"`
	SnoozeUntil          string          `json:"snooze_until,omitempty"`
	Snoozed              bool            `json:"snoozed,omitempty"`
	Visible              bool            `json:"visible,omitempty"`
	Unlocked             bool            `json:"unlocked,omitempty"`
	ActivityLabel        string          `json:"activity_label,omitempty"`
	HermesProfile        string          `json:"hermes_profile,omitempty"`
	Language             string          `json:"language,omitempty"`
	SpeechRequestID      string          `json:"speech_request_id,omitempty"`
	ContentFingerprint   string          `json:"content_fingerprint,omitempty"`
	IntervalMS           int             `json:"interval_ms,omitempty"`
	Enabled              *bool           `json:"enabled,omitempty"`
	Instance             string          `json:"instance,omitempty"`
	SDP                  string          `json:"sdp,omitempty"`
	Candidate            string          `json:"candidate,omitempty"`
	SDPMid               string          `json:"sdp_mid,omitempty"`
	SDPMLineIndex        int             `json:"sdp_mline_index,omitempty"`
	ReceiptSequence      uint64          `json:"-"`
	ReceivedAt           time.Time       `json:"-"`
	Raw                  json.RawMessage `json:"-"`
	Invalid              bool            `json:"-"`
}

func DecodeFrame(frame []byte) (Inbound, error) {
	if !utf8.Valid(frame) {
		return Inbound{}, errors.New("message is not valid UTF-8")
	}
	if trimmed := bytes.TrimLeft(frame, " \t\r\n"); len(trimmed) == 0 || trimmed[0] != '{' {
		return Inbound{}, errors.New("message must be a JSON object")
	}
	var message Inbound
	if err := json.Unmarshal(frame, &message); err != nil {
		var syntaxErr *json.SyntaxError
		if errors.As(err, &syntaxErr) {
			return Inbound{}, err
		}
		message.Invalid = true
	}
	if message.Type == "" {
		message.Invalid = true
	}
	message.Raw = frame
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
	if known {
		metadata.Operation = operation
	}
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

func DecodeFailureResponse(requestID string) map[string]any {
	return ErrorResponse(requestID, NewApiError(ErrorInvalidRequest, nil))
}

type PushConfig struct {
	Type           string `json:"type"`
	VAPIDPublicKey string `json:"vapid_public_key"`
	Host           string `json:"host"`
	// Home lets the phone print a checkout as "~/code/app" instead of the
	// computer's absolute path, which rarely fits a phone row.
	Home           string             `json:"home"`
	Protocol       int                `json:"protocol"`
	Version        string             `json:"version"`
	ReleaseVersion string             `json:"release_version"`
	Revision       string             `json:"revision"`
	Update         any                `json:"update"`
	AppDeploy      any                `json:"app_deploy"`
	Capabilities   []string           `json:"capabilities"`
	HerdrStatus    herdr.ServerStatus `json:"herdr_status"`
	// SpeechLanguages lists the languages this host has a voice for, so the
	// phone offers only what the relay can actually read aloud.
	SpeechLanguages []string `json:"speech_languages,omitempty"`
	Inventory       any      `json:"inventory"`
	AgentProfiles   any      `json:"agent_profiles"`
	// HermesProfiles lists the Hermes Agent profiles a launch may select, so
	// the phone's Hermes form offers only names this host can actually start.
	// Omitted by relays that do not know about profiles at all.
	HermesProfiles any `json:"hermes_profiles,omitempty"`
	// Hybrid advertises the gateway + direct WebRTC descriptor to an app that
	// connected over the legacy WSS URL, so the bridge window needs no QR
	// re-scan. Omitted entirely when the relay has no gateway configured.
	Hybrid map[string]any `json:"hybrid,omitempty"`
}

const AgentResponseCopyCapability = "agent_response_copy"

const SpeechSynthesisCapability = "speech_synthesis"

// PaneSizeOptInCapability is advertised only by relays that can resize a pane
// on platforms with a backend. It replaces pane_size_lease, which apps up to
// 0.22.7 treated as permission to lease automatically; leases must now carry
// opt_in. PaneSizeOptInRowsCapability adds height leasing.
const PaneSizeOptInCapability = "pane_size_opt_in"
const PaneSizeOptInRowsCapability = "pane_size_opt_in_rows"
const SpeechVoiceManagementCapability = "speech_voice_management"

var Capabilities = []string{
	"attention_classification",
	"clear_activities",
	"directory_browser",
	"self_update",
	"structured_questions",
	"slash_commands",
	"conversation_history",
	"conversation_activity",
	"workspace_inspection",
	"invitation_qr",
	"paging_keys",
}
