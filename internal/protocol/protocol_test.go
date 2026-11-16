package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDecodeCanonicalOperation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		frame   string
		want    string
		invalid bool
	}{
		{"type only", `{"type":"send_secret"}`, "send_secret", false},
		{"type determines action", `{"type":"send_secret","action":"refresh_agents"}`, "send_secret", false},
		{"action only", `{"action":"send_secret"}`, "", true},
		{"command envelope", `{"type":"command","action":"send_secret"}`, "command", false},
		{"missing", `{}`, "", true},
		{"unknown", `{"type":"unknown"}`, "unknown", false},
		{"nonstring type", `{"type":1,"action":"send_secret"}`, "", true},
		{"nonstring action ignored", `{"type":"send_secret","action":1}`, "send_secret", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeFrame([]byte(tc.frame))
			if err != nil || got.Invalid != tc.invalid || got.Type != tc.want {
				t.Fatalf("DecodeFrame = %q, invalid=%v, %v; want %q, invalid=%v", got.Type, got.Invalid, err, tc.want, tc.invalid)
			}
		})
	}
}

func TestDecodeFrameRefusesMalformedFrames(t *testing.T) {
	for name, frame := range map[string]string{
		"invalid UTF-8":  "{\"type\":\"agent_stop\",\"text\":\"\xff\"}",
		"malformed JSON": `{"type":`,
		"truncated":      `{"type":"agent_stop"`,
		"null":           `null`,
		"array":          `[{"type":"agent_stop"}]`,
		"string":         `"agent_stop"`,
		"empty":          ``,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeFrame([]byte(frame)); err == nil {
				t.Fatalf("DecodeFrame(%q) succeeded", frame)
			}
		})
	}
}

func TestDecodeFrameKeepsRequestIDOfInvalidFrames(t *testing.T) {
	for name, tc := range map[string]struct {
		frame     string
		requestID string
	}{
		"wrong typed field":      {`{"type":"read_pane","request_id":"r1","lines":"many"}`, "r1"},
		"missing type":           {`{"request_id":"r2"}`, "r2"},
		"wrong typed request id": {`{"type":"read_pane","request_id":7,"lines":1}`, ""},
		"fractional number":      {`{"type":"read_pane","request_id":"r3","lines":30.0}`, "r3"},
		"exponent number":        {`{"type":"tab_reorder","request_id":"r4","insert_index":1e1}`, "r4"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := DecodeFrame([]byte(tc.frame))
			if err != nil || !got.Invalid || got.RequestID != tc.requestID {
				t.Fatalf("DecodeFrame = %+v, %v; want invalid with request id %q", got, err, tc.requestID)
			}
		})
	}
}

func TestDecodeFrameKeepsTheFrame(t *testing.T) {
	frame := []byte(` {"type":"agent_stop","request_id":"r1"} `)
	got, err := DecodeFrame(frame)
	if err != nil || got.Invalid || string(got.Raw) != string(frame) {
		t.Fatalf("DecodeFrame = %+v, %v", got, err)
	}
}

func TestDecodeFrameTakesTheLastOfTwoMatchingKeys(t *testing.T) {
	got, err := DecodeFrame([]byte(`{"type":"send_keys","keys":["a"],"Keys":["b"]}`))
	if err != nil || len(got.Keys) != 1 || got.Keys[0] != "b" {
		t.Fatalf("keys = %v, %v; want [b]", got.Keys, err)
	}
}

func TestClientCannotSetServerFields(t *testing.T) {
	frame := `{"type":"agent_stop","_server_sequence":9,"_server_received_at":"2026-01-02T03:04:05Z","ReceiptSequence":9,"receipt_sequence":9,"ReceivedAt":"2026-01-02T03:04:05Z","received_at":"2026-01-02T03:04:05Z","Raw":"x","raw":"x","Invalid":true,"invalid":true}`
	got, err := DecodeFrame([]byte(frame))
	if err != nil {
		t.Fatal(err)
	}
	if got.ReceiptSequence != 0 || !got.ReceivedAt.IsZero() || got.Invalid || string(got.Raw) != frame {
		t.Fatalf("decoded = %+v; want no server fields", got)
	}
}

func TestInputAndLifecycleActionClassification(t *testing.T) {
	for _, tc := range []struct {
		action string
		known  bool
	}{
		{"send_text", false},
		{"agent_restart", false},
		{"send_input", true},
		{"submit_prompt", true},
		{"agent_clear", true},
	} {
		t.Run(tc.action, func(t *testing.T) {
			metadata, known := ClassifyAction(tc.action)
			if known != tc.known {
				t.Fatalf("ClassifyAction(%q) known = %v, want %v", tc.action, known, tc.known)
			}
			if known && (metadata.Class != ActionMutating || !metadata.Coordinated || !metadata.Audited) {
				t.Fatalf("mutation metadata = %+v", metadata)
			}
		})
	}
}

func TestEveryInboundFixtureDecodesAsV3Action(t *testing.T) {
	root := filepath.Join("testdata", "inbound")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(inboundFixtureFields) {
		t.Fatalf("%d inbound fixtures, %d expectations", len(entries), len(inboundFixtureFields))
	}
	for _, entry := range entries {
		want, expected := inboundFixtureFields[entry.Name()]
		if !expected {
			t.Fatalf("%s: no expected fields", entry.Name())
		}
		data, err := os.ReadFile(filepath.Join(root, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		message, err := DecodeFrame(data)
		if err != nil || message.Invalid {
			t.Fatalf("%s: %+v, %v", entry.Name(), message, err)
		}
		if string(message.Raw) != string(data) {
			t.Fatalf("%s: raw frame was not kept", entry.Name())
		}
		message.Raw = nil
		if message.Subscription != nil {
			var compact bytes.Buffer
			if err := json.Compact(&compact, message.Subscription); err != nil {
				t.Fatalf("%s: %v", entry.Name(), err)
			}
			message.Subscription = compact.Bytes()
		}
		if !reflect.DeepEqual(message, want) {
			t.Fatalf("%s: decoded %+v, want %+v", entry.Name(), message, want)
		}
		if _, known := ClassifyAction(message.Type); !known {
			t.Fatalf("%s: action %q is not classified", entry.Name(), message.Type)
		}
		if !Compatible(message) {
			t.Fatalf("%s: compatible v3 action was rejected", entry.Name())
		}
	}
}

func intPointer(value int) *int { return &value }

var uploadTarget = &TargetRef{ServerSessionID: "primary", PaneID: "pane-1", TerminalID: "terminal-1", Generation: 1}

var inboundFixtureFields = map[string]Inbound{
	"acknowledge_pane.json": {Type: "acknowledge_pane", RequestID: "req-006", Protocol: 3, PaneID: "pane-1"},
	"agent_clear.json":      {Type: "agent_clear", RequestID: "req-005", Protocol: 3, PaneID: "pane-1"},
	"agent_rename.json":     {Type: "agent_rename", RequestID: "req-rename", Protocol: 3, PaneID: "pane-1", Name: "123"},
	"agent_start.json": {
		Type: "agent_start", RequestID: "req-004", Protocol: 3, ProfileID: "claude", Name: "fix-bug",
		Cwd: "/home/user/project", WorkspaceID: "w1", Prompt: "Fix the failing test",
	},
	"agent_stop.json": {Type: "agent_stop", RequestID: "req-stop", Protocol: 3, PaneID: "pane-1"},
	"answer_question.json": {
		Type: "answer_question", RequestID: "req-question-answer", Protocol: 3, PaneID: "pane-1",
		InteractionID: "interaction-1", SelectedIndices: []int{0},
	},
	"approval.json": {
		Type: "respond", RequestID: "req-003", Protocol: 3, PaneID: "pane-1", EventID: "evt-abc123",
		ApprovalFingerprint: "5105fbe5b6e54a57b38dbf8bd73848409bbcfacbb4c055879bdb8e30c7cc46f8",
		Choice:              "Approve once", Index: intPointer(0), Total: intPointer(2),
	},
	"cancel_speech.json": {
		Type: "cancel_speech", Protocol: 3, PaneID: "pane-1", SpeechRequestID: "speech-1",
		Target: &TargetRef{ServerSessionID: "primary", PaneID: "pane-1", TerminalID: "terminal-1", Generation: 4, AgentSessionID: "agent-session-1"},
	},
	"check_update.json": {Type: "check_update", RequestID: "req-check-update"},
	"clarify_question.json": {
		Type: "clarify_question", RequestID: "req-question-chat", Protocol: 3, PaneID: "pane-1", InteractionID: "interaction-3",
	},
	"clear_activities.json": {Type: "clear_activities", RequestID: "req-clear-activities", Protocol: 3},
	"deploy_app_update.json": {
		Type: "deploy_app_update", RequestID: "req-deploy", Protocol: 3, ExpectedVersion: "0.9.0",
		ExpectedRevision: "0123456789abcdef0123456789abcdef01234567", ExpectedOrigin: "https://app.example.test",
	},
	"get_activity.json": {Type: "get_activity", Limit: 500},
	"install_update.json": {
		Type: "install_update", RequestID: "req-install-update", ExpectedVersion: "0.9.0",
		ExpectedRevision: "0123456789abcdef0123456789abcdef01234567", ExpectedOrigin: "https://app.example.test",
	},
	"keys.json":             {Type: "send_keys", RequestID: "req-002", Protocol: 3, PaneID: "pane-1", Keys: []string{"Enter"}},
	"lease_pane_size.json":  {Type: "lease_pane_size", RequestID: "req-lease-size", Protocol: 3, PaneID: "pane-1", Columns: 84, Rows: 48, OptIn: true},
	"list_directories.json": {Type: "list_directories", RequestID: "req-directories", Path: "/home/user/project"},
	"list_slash_commands.json": {
		Type: "list_slash_commands", RequestID: "req-commands", PaneID: "pane-1",
	},
	"navigate_question.json": {
		Type: "navigate_question", RequestID: "req-question-navigate", Protocol: 3, PaneID: "pane-1",
		InteractionID: "interaction-2", Direction: "previous",
	},
	"prompt.json": {Type: "submit_prompt", RequestID: "req-001", Protocol: 3, PaneID: "pane-1", Text: "Hello, fix the bug in main.go"},
	"push_subscribe.json": {
		Type: "push_subscribe", Protocol: 3, ClientID: "device-1:relay-1", ReplaceEndpoints: []string{},
		Subscription: json.RawMessage(`{"endpoint":"https://push.example.test/subscription","keys":{"p256dh":"test-p256dh","auth":"test-auth"}}`),
	},
	"push_unsubscribe.json": {
		Type: "push_unsubscribe", Protocol: 3, ClientID: "device-1:relay-1", Endpoints: []string{"https://push.example.test/subscription"},
	},
	"read_pane.json":           {Type: "read_pane", RequestID: "req-read", PaneID: "pane-1", Lines: intPointer(80), Format: "ansi"},
	"refresh_agents.json":      {Type: "refresh_agents"},
	"register_app_origin.json": {Type: "register_app_origin", Protocol: 3, Origin: "https://app.example.test"},
	"release_pane_size.json":   {Type: "release_pane_size", RequestID: "req-release-size", Protocol: 3, PaneID: "pane-1"},
	"send_secret.json":         {Type: "send_secret", RequestID: "req-secret", Protocol: 3, PaneID: "pane-1", Text: "correct horse"},
	"send_terminal_text.json": {
		Type: "send_terminal_text", RequestID: "req-terminal-text", Protocol: 3, PaneID: "pane-1",
		Target: &TargetRef{ServerSessionID: "primary", PaneID: "pane-1", TerminalID: "terminal-1", Generation: 1, AgentSessionID: " opaque/session 界 "},
		Text:   "  /arbitrary 界😀  ",
	},
	"tab_reorder.json":   {Type: "tab_reorder", RequestID: "req-tab-reorder", Protocol: 3, PaneID: "pane-1", InsertIndex: intPointer(2)},
	"upload_begin.json":  {Type: "upload_begin", RequestID: "req-upload-begin", Protocol: 3, Target: uploadTarget},
	"upload_cancel.json": {Type: "upload_cancel", RequestID: "req-upload-cancel", Protocol: 3, Target: uploadTarget},
	"upload_chunk.json": {
		Type: "upload_chunk", RequestID: "req-upload-chunk", Protocol: 3, Target: uploadTarget, Data: "iVBORw0KGgo=",
	},
	"upload_finish.json":    {Type: "upload_finish", RequestID: "req-upload-finish", Protocol: 3, Target: uploadTarget},
	"workspace_close.json":  {Type: "workspace_close", RequestID: "req-workspace-close", Protocol: 3, WorkspaceID: "w1"},
	"workspace_create.json": {Type: "workspace_create", RequestID: "req-workspace-create", Protocol: 3, Cwd: "/home/user/project", Label: "Project"},
	"workspace_rename.json": {Type: "workspace_rename", RequestID: "req-workspace-rename", Protocol: 3, WorkspaceID: "w1", Label: "Renamed Project"},
	"workspace_reorder.json": {
		Type: "workspace_reorder", RequestID: "req-workspace-reorder", Protocol: 3, WorkspaceIDs: []string{"w1", "w2"}, BeforeWorkspaceID: "w5",
	},
	"worktree_create.json": {
		Type: "worktree_create", RequestID: "req-worktree-create", Protocol: 3, WorkspaceID: "w1", Branch: "fix/issue-14", Base: "main", Label: "Issue 14",
	},
	"worktree_list.json": {Type: "worktree_list", RequestID: "req-worktree-list", Protocol: 3, WorkspaceID: "w1"},
	"worktree_open.json": {
		Type: "worktree_open", RequestID: "req-worktree-open", Protocol: 3, WorkspaceID: "w1",
		Path: "/home/user/worktrees/project/issue-14", Label: "Issue 14",
	},
	"worktree_remove.json": {Type: "worktree_remove", RequestID: "req-worktree-remove", Protocol: 3, WorkspaceID: "w2"},
}

func TestUnversionedActionCannotBypassProtocolGate(t *testing.T) {
	message, err := DecodeFrame([]byte(`{"type":"agent_stop","request_id":"unversioned","pane_id":"pane-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if message.Type != "agent_stop" || Compatible(message) {
		t.Fatalf("unversioned envelope decoded as %+v", message)
	}
	response := IncompatibleResponse(message)
	receipt, ok := response["receipt"].(ActionReceipt)
	if response["type"] != "action_receipt" || !ok || receipt.Phase != ReceiptFailedBeforeDispatch {
		t.Fatalf("incompatible response = %+v", response)
	}
}

func TestInstallUpdateRemainsBootstrapCompatible(t *testing.T) {
	message, err := DecodeFrame([]byte(`{"type":"install_update"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !Compatible(message) {
		t.Fatal("install_update bootstrap unexpectedly requires protocol v3")
	}
}

func TestPushConfigFixtureHasRequiredCutoverFields(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "outbound", "push_config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture PushConfig
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Type != "push_config" || fixture.Protocol != Version ||
		fixture.Version == "" || fixture.ReleaseVersion == "" || fixture.Revision == "" ||
		fixture.Update == nil || fixture.AppDeploy == nil ||
		fixture.HerdrStatus.Generation == 0 || len(fixture.HerdrStatus.Features) == 0 {
		t.Fatalf("incomplete push_config fixture: %+v", fixture)
	}
	fixtureCapabilities := make(map[string]bool, len(fixture.Capabilities))
	for _, capability := range fixture.Capabilities {
		fixtureCapabilities[capability] = true
	}
	for _, capability := range Capabilities {
		if !fixtureCapabilities[capability] {
			t.Fatalf("push_config fixture is missing base capability %q", capability)
		}
	}
	foundAttentionClassification := false
	for _, capability := range fixture.Capabilities {
		foundAttentionClassification = foundAttentionClassification ||
			capability == "attention_classification"
	}
	if !foundAttentionClassification {
		t.Fatal("push_config fixture does not advertise attention_classification")
	}
}

func TestDecodeFailureUsesStableError(t *testing.T) {
	response := DecodeFailureResponse("r1")
	apiError, ok := response["error"].(ApiError)
	if response["type"] != "error" || response["request_id"] != "r1" || !ok ||
		apiError.Code != "invalid_request" {
		t.Fatalf("decode failure = %+v", response)
	}
}

func TestSharedDTOJSON(t *testing.T) {

	page := OpaquePage[string]{
		Items:       []string{"one"},
		Truncated:   false,
		GeneratedAt: 1700000000000,
	}
	encoded, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"items":["one"],"truncated":false,"generated_at":1700000000000}` {
		t.Fatalf("OpaquePage JSON = %s", encoded)
	}
}

func TestDeviceAndReceiptJSON(t *testing.T) {
	device := DeviceContext{
		DeviceID:          "device-1",
		CredentialID:      "credential-1",
		Role:              RoleController,
		Locale:            "en-US",
		CredentialVersion: 3,
	}
	encoded, err := json.Marshal(device)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"device_id":"device-1","credential_id":"credential-1","role":"controller","locale":"en-US","credential_version":3}` {
		t.Fatalf("DeviceContext JSON = %s", encoded)
	}

	apiError := NewApiError(ErrorInvalidRequest, nil)
	response := ActionReceiptResponse("request-1", ActionReceipt{
		ActionID: "action-1",
		Phase:    ReceiptFailedBeforeDispatch,
		Error:    &apiError,
	})
	encoded, err = json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"receipt":{"action_id":"action-1","phase":"failed_before_dispatch","error":{"code":"invalid_request"}},"request_id":"request-1","type":"action_receipt"}` {
		t.Fatalf("ActionReceipt response JSON = %s", encoded)
	}
}

func TestInboundDecodesCompositeScope(t *testing.T) {
	raw := map[string]any{
		"type":              "submit_prompt",
		"protocol":          Version,
		"request_id":        "request-1",
		"action_id":         "action-1",
		"server_session_id": "primary",
		"target": map[string]any{
			"server_session_id": "primary",
			"pane_id":           "pane-1",
			"terminal_id":       "terminal-1",
			"generation":        2,
		},
	}
	frame, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	inbound, err := DecodeFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	scope, known := ScopeFor(inbound)
	if !known || scope.Action.Operation != "submit_prompt" || scope.ActionID != "action-1" ||
		scope.ServerSessionID != "primary" || scope.Target == nil || scope.Target.PaneID != "pane-1" {
		t.Fatalf("scope = %+v, known = %t", scope, known)
	}
}

func TestActionCatalogMetadataIsClosed(t *testing.T) {
	for operation := range actionCatalog {
		metadata, known := ClassifyAction(operation)
		if !known {
			t.Errorf("%q is not classified", operation)
		}
		if metadata.Operation != operation {
			t.Errorf("%q metadata operation = %q", operation, metadata.Operation)
		}
		if metadata.Class != ActionReadOnly && metadata.Class != ActionMutating {
			t.Errorf("%q has invalid class %q", operation, metadata.Class)
		}
		if metadata.Class == ActionReadOnly && (metadata.RequiresProtocol || metadata.Coordinated || metadata.Audited) {
			t.Errorf("%q read-only metadata has mutation flags: %+v", operation, metadata)
		}
	}
}

func TestActionClassificationFailsClosed(t *testing.T) {
	tests := []struct {
		operation   string
		class       ActionClass
		coordinated bool
		audited     bool
	}{
		{operation: "read_pane", class: ActionReadOnly},
		{operation: "submit_prompt", class: ActionMutating, coordinated: true, audited: true},
		{operation: "copy_agent_response", class: ActionMutating},
		{operation: "workspace_tree", class: ActionReadOnly},
	}
	for _, test := range tests {
		metadata, known := ClassifyAction(test.operation)
		if !known || metadata.Class != test.class || metadata.Coordinated != test.coordinated ||
			metadata.Audited != test.audited {
			t.Errorf("ClassifyAction(%q) = (%+v, %t)", test.operation, metadata, known)
		}
	}
	if _, known := ClassifyAction("future_unregistered_action"); known {
		t.Fatal("unknown action was classified")
	}
	if !RequiresProtocol("future_unregistered_action") {
		t.Fatal("unknown action did not fail closed")
	}
}

func TestApiErrorArgumentsAreBounded(t *testing.T) {
	args := make(map[string]any)
	for index := range 12 {
		args[fmt.Sprintf("ARG_%02d", index)] = strings.Repeat("界", 200)
	}
	args["unsupported"] = []string{"not", "scalar"}
	apiError := NewApiError(" UNKNOWN_ACTION ", args)
	if apiError.Code != "unknown_action" || len(apiError.Args) > maxErrorArgs {
		t.Fatalf("ApiError = %+v", apiError)
	}
	encoded, err := json.Marshal(apiError.Args)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > maxErrorArgsJSON {
		t.Fatalf("ApiError args encoded to %d bytes", len(encoded))
	}
	for key, value := range apiError.Args {
		if key != strings.ToLower(key) || len(key) > maxErrorArgKey {
			t.Errorf("unbounded argument key %q", key)
		}
		if text, ok := value.(string); ok && len(text) > maxErrorArgString {
			t.Errorf("unbounded argument value is %d bytes", len(text))
		}
	}
}

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
