package protocol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestEveryInboundFixtureDecodesAndCoversMutations(t *testing.T) {
	root := filepath.Join("..", "..", "contracts", "fixtures", "inbound")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	covered := make(map[string]bool)
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
		if RequiresProtocol(message.Type) && !Compatible(message) {
			t.Fatalf("%s: compatible v2 fixture was rejected", entry.Name())
		}
		covered[message.Type] = true
	}
	for messageType := range mutatingTypes {
		if messageType == "install_update" {
			continue
		}
		if !covered[messageType] {
			t.Errorf("mutating message %q has no frozen inbound fixture", messageType)
		}
	}
}

func TestLegacyCommandEnvelopeCannotBypassProtocolGate(t *testing.T) {
	message, err := DecodeMap(map[string]any{
		"type":       "command",
		"action":     "agent_stop",
		"request_id": "legacy",
		"pane_id":    "pane-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if message.Type != "agent_stop" || Compatible(message) {
		t.Fatalf("legacy envelope decoded as %+v", message)
	}
	response := IncompatibleResponse(message)
	if response["type"] != "command_result" || response["action"] != "agent_stop" {
		t.Fatalf("incompatible response = %+v", response)
	}
}

func TestInstallUpdateRemainsBootstrapCompatible(t *testing.T) {
	message, err := DecodeMap(map[string]any{"type": "install_update"})
	if err != nil {
		t.Fatal(err)
	}
	if !Compatible(message) {
		t.Fatal("install_update bootstrap unexpectedly requires protocol v2")
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
		fixture.Update == nil || fixture.AppDeploy == nil ||
		len(fixture.Capabilities) != len(Capabilities) {
		t.Fatalf("incomplete push_config fixture: %+v", fixture)
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

func TestDecodeFailurePreservesTypeSpecificResponse(t *testing.T) {
	upload := DecodeFailureResponse(map[string]any{
		"type": "upload_image", "request_id": "u1", "pane_id": "p1",
	})
	if upload["type"] != "upload_result" || upload["request_id"] != "u1" || upload["pane_id"] != "p1" {
		t.Fatalf("upload failure = %+v", upload)
	}
	subscribe := DecodeFailureResponse(map[string]any{"type": "push_subscribe"})
	if subscribe["type"] != "push_subscribed" || subscribe["ok"] != false {
		t.Fatalf("subscribe failure = %+v", subscribe)
	}
	command := DecodeFailureResponse(map[string]any{
		"type": "command", "action": "agent_start", "request_id": "r1",
	})
	if command["type"] != "command_result" || command["action"] != "agent_start" {
		t.Fatalf("command failure = %+v", command)
	}
}
