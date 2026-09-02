package protocol

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEveryInboundFixtureDecodesAsV3Action(t *testing.T) {
	root := filepath.Join("..", "..", "contracts", "fixtures", "inbound")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]any
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatalf("%s: %v", entry.Name(), err)
		}
		message, err := DecodeMap(raw)
		if err != nil {
			t.Fatalf("%s: %v", entry.Name(), err)
		}
		if _, known := ClassifyAction(message.Type); !known {
			t.Fatalf("%s: action %q is not classified", entry.Name(), message.Type)
		}
		if !Compatible(message) {
			t.Fatalf("%s: compatible v3 action was rejected", entry.Name())
		}
	}
}

func TestCommandEnvelopeCannotBypassProtocolGate(t *testing.T) {
	message, err := DecodeMap(map[string]any{
		"type":       "command",
		"action":     "agent_stop",
		"request_id": "unversioned",
		"pane_id":    "pane-1",
	})
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
	message, err := DecodeMap(map[string]any{"type": "install_update"})
	if err != nil {
		t.Fatal(err)
	}
	if !Compatible(message) {
		t.Fatal("install_update bootstrap unexpectedly requires protocol v3")
	}
}

func TestPushConfigFixtureHasRequiredCutoverFields(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "contracts", "fixtures", "outbound", "push_config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture PushConfig
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Type != "push_config" || fixture.Protocol != Version ||
		fixture.Version == "" || fixture.ReleaseVersion == "" || fixture.Revision == "" ||
		fixture.Update == nil || fixture.AppDeploy == nil {
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
	response := DecodeFailureResponse(map[string]any{
		"type": "command", "action": "agent_start", "request_id": "r1",
	})
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
		"type":              "command",
		"action":            "submit_prompt",
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
	inbound, err := DecodeMap(raw)
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
	for operation, metadata := range actionCatalog {
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
