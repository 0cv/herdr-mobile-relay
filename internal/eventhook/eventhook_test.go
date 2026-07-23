package eventhook

import (
	"path/filepath"
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
