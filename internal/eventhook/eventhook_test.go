package eventhook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildPayload(t *testing.T) {
	t.Setenv("HERDR_SOCKET_PATH", filepath.Join(t.TempDir(), "herdr.sock"))
	number := 2
	payload, err := Build(EventData{
		PaneID:       "pane-1",
		TabID:        "tab-1",
		TabName:      "Main",
		TabNumber:    &number,
		WorkspaceID:  "ws-1",
		AgentStatus:  "BLOCKED",
		DisplayAgent: "Claude",
		Cwd:          "/tmp/project",
	})
	if err != nil {
		t.Fatal(err)
	}
	if payload.Type != "agent_event" || payload.Status != "blocked" || payload.Agent != "claude" {
		t.Fatalf("payload = %#v", payload)
	}
	if payload.Project != "project" || payload.TabLabel != "Main" {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestLoadEnvironmentReadsRelayEnvAndKeepsExistingValues(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "relay.env")
	if err := os.WriteFile(envFile, []byte("HERDR_SOCKET_PATH='/from/file.sock'\nexport HERDR_RELAY_PLUGIN_PORT=\"9001\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_RELAY_ENV", envFile)
	t.Setenv("HERDR_SOCKET_PATH", "")
	t.Setenv("HERDR_RELAY_PLUGIN_PORT", "")
	os.Unsetenv("HERDR_RELAY_PLUGIN_PORT")

	if err := loadEnvironment(); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("HERDR_SOCKET_PATH"); got != "" {
		t.Fatalf("existing HERDR_SOCKET_PATH overridden: %q", got)
	}
	if got := os.Getenv("HERDR_RELAY_PLUGIN_PORT"); got != "9001" {
		t.Fatalf("HERDR_RELAY_PLUGIN_PORT = %q", got)
	}
}

func TestLoadEnvironmentRejectsVariableReference(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "relay.env")
	if err := os.WriteFile(envFile, []byte("HERDR_RELAY_PLUGIN_PORT=$PORT\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_RELAY_ENV", envFile)
	t.Setenv("HERDR_RELAY_PLUGIN_PORT", "")
	os.Unsetenv("HERDR_RELAY_PLUGIN_PORT")

	err := loadEnvironment()
	if err == nil || !strings.Contains(err.Error(), "HERDR_RELAY_PLUGIN_PORT") {
		t.Fatalf("error = %v, want one naming HERDR_RELAY_PLUGIN_PORT", err)
	}
}
