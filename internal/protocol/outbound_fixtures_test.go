package protocol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/activity"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/slashcmd"
)

func TestAgentsFixtureMatchesGoType(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "contracts", "fixtures", "outbound", "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	var msg struct {
		Type   string                   `json:"type"`
		Agents []coordinator.AgentState `json:"agents"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Type != "agents" {
		t.Errorf("type = %q, want agents", msg.Type)
	}
	if len(msg.Agents) == 0 {
		t.Fatal("agents array is empty")
	}
	for _, agent := range msg.Agents {
		if agent.PaneID == "" {
			t.Error("agent has empty pane_id")
		}
		if agent.UpdatedAt != 0 && agent.UpdatedAt < 1_000_000_000_000 {
			t.Errorf("updated_at = %d, want 0 (initial snapshot) or epoch milliseconds (>1e12)", agent.UpdatedAt)
		}
	}
	serialized, err := json.Marshal(struct {
		Type   string                   `json:"type"`
		Agents []coordinator.AgentState `json:"agents"`
	}{Type: msg.Type, Agents: msg.Agents})
	if err != nil {
		t.Fatal(err)
	}
	var fixtureValue, serializedValue any
	if err := json.Unmarshal(data, &fixtureValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(serialized, &serializedValue); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(serializedValue, fixtureValue) {
		t.Fatalf("agents Go serialization differs from exact fixture\nserialized: %s\nfixture: %s", serialized, data)
	}
}

func TestCommandResultFixtureIsExact(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "contracts", "fixtures", "outbound", "command_result.json"))
	if err != nil {
		t.Fatal(err)
	}
	var actual map[string]any
	if err := json.Unmarshal(data, &actual); err != nil {
		t.Fatal(err)
	}
	expected := map[string]any{
		"type":       "command_result",
		"request_id": "req-001",
		"action":     "prompt",
		"ok":         true,
		"phase":      "completed",
		"error":      "",
		"pane_id":    "pane-1",
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("command_result fixture = %#v, want %#v", actual, expected)
	}
}

func TestQuestionResultFixturesCoverTerminalProtocol(t *testing.T) {
	tests := []struct {
		name    string
		phase   string
		ok      bool
		hasData bool
	}{
		{name: "question_navigated.json", phase: "navigated", ok: true, hasData: true},
		{name: "question_advanced.json", phase: "advanced", ok: true, hasData: true},
		{name: "question_unconfirmed.json", phase: "unconfirmed", ok: false, hasData: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("..", "..", "contracts", "fixtures", "outbound", test.name))
			if err != nil {
				t.Fatal(err)
			}
			var envelope struct {
				Type string `json:"type"`
				coordinator.CommandResult
			}
			if err := json.Unmarshal(data, &envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Type != "command_result" || envelope.Phase != test.phase ||
				envelope.OK != test.ok || (envelope.Data != nil) != test.hasData {
				t.Fatalf("question result fixture = %+v", envelope)
			}
		})
	}
}

func TestSlashCommandsFixtureMatchesCompleteClaudeCatalog(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "contracts", "fixtures", "outbound", "slash_commands.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Type      string           `json:"type"`
		RequestID string           `json:"request_id"`
		Action    string           `json:"action"`
		OK        bool             `json:"ok"`
		Phase     string           `json:"phase"`
		Error     string           `json:"error"`
		PaneID    string           `json:"pane_id"`
		Data      slashcmd.Catalog `json:"data"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	actual := slashcmd.CatalogFor("claude", "/nonexistent", "/nonexistent")
	if fixture.Type != "command_result" || fixture.RequestID != "req-slash-1" ||
		fixture.Action != "list_slash_commands" || !fixture.OK ||
		fixture.Phase != "completed" || fixture.Error != "" || fixture.PaneID != "pane-1" ||
		!reflect.DeepEqual(fixture.Data, actual) {
		t.Fatalf("slash fixture does not match complete wire catalog\nfixture: %#v\nactual: %#v", fixture, actual)
	}
}

func TestAgentUpdateFixtureMatchesGoType(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "contracts", "fixtures", "outbound", "agent_update.json"))
	if err != nil {
		t.Fatal(err)
	}
	var msg struct {
		Type string `json:"type"`
		coordinator.AgentState
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Type != "agent_update" {
		t.Errorf("type = %q, want agent_update", msg.Type)
	}
	if msg.PaneID == "" {
		t.Error("agent_update has empty pane_id")
	}
	if msg.UpdatedAt < 1_000_000_000_000 {
		t.Errorf("updated_at = %d, want epoch milliseconds (>1e12)", msg.UpdatedAt)
	}
}

func TestActivityHistoryFixtureMatchesGoType(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "contracts", "fixtures", "outbound", "activity_history.json"))
	if err != nil {
		t.Fatal(err)
	}
	var msg struct {
		Type       string           `json:"type"`
		Activities []activity.Entry `json:"activities"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Type != "activity_history" {
		t.Errorf("type = %q, want activity_history", msg.Type)
	}
	if len(msg.Activities) == 0 {
		t.Fatal("activities array is empty")
	}
	for _, entry := range msg.Activities {
		if entry.ID == "" {
			t.Error("activity has empty id")
		}
		if int64(entry.Timestamp) < 1_000_000_000_000 {
			t.Errorf("timestamp = %d, want epoch milliseconds (>1e12)", entry.Timestamp)
		}
		if entry.Kind == "" {
			t.Error("activity has empty kind")
		}
		if entry.Summary == "" {
			t.Error("activity has empty summary")
		}
	}
}

func TestOutboundFixturesUseConsistentTimestampUnit(t *testing.T) {
	agentsData, err := os.ReadFile(filepath.Join("..", "..", "contracts", "fixtures", "outbound", "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	updateData, err := os.ReadFile(filepath.Join("..", "..", "contracts", "fixtures", "outbound", "agent_update.json"))
	if err != nil {
		t.Fatal(err)
	}

	var agents struct {
		Agents []struct {
			UpdatedAt int64 `json:"updated_at"`
		} `json:"agents"`
	}
	var update struct {
		UpdatedAt int64 `json:"updated_at"`
	}
	if err := json.Unmarshal(agentsData, &agents); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(updateData, &update); err != nil {
		t.Fatal(err)
	}

	for _, a := range agents.Agents {
		if a.UpdatedAt != 0 && a.UpdatedAt < 1_000_000_000_000 {
			t.Errorf("agents.json updated_at = %d looks like seconds, want 0 or milliseconds", a.UpdatedAt)
		}
	}
	if update.UpdatedAt < 1_000_000_000_000 {
		t.Errorf("agent_update.json updated_at = %d looks like seconds, want milliseconds", update.UpdatedAt)
	}
}
