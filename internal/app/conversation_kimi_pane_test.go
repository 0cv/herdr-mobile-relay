package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/backend"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

const kimiVisiblePaneText = "visible pane text from the terminal"

type visiblePaneBackend struct {
	backend.Backend
}

func (visiblePaneBackend) ReadPane(context.Context, string, int, string) (herdr.PaneRead, error) {
	return herdr.PaneRead{Content: []byte(kimiVisiblePaneText)}, nil
}

func kimiRelayChosenPane(t *testing.T) (*Server, *coordinator.AgentState, string) {
	t.Helper()
	server, agent, wire := kimiE2dDuplicateSource(t)
	server.herdrC = visiblePaneBackend{Backend: server.herdrC}
	return server, agent, wire
}

func kimiReportedPane(t *testing.T) (*Server, *coordinator.AgentState, string) {
	t.Helper()
	server, agent, wire := kimiIdentityServer(t)
	server.herdrC = visiblePaneBackend{Backend: server.herdrC}
	agent.ConversationInferred, agent.ConversationSource, agent.ConversationInferredOwner = false, "", ""
	server.state.CommitInventory([]*coordinator.AgentState{agent}, server.state.RevisionCounter())
	reported, _ := server.state.Agent(agent.PaneID)
	return server, reported, wire
}

func captureKimiPane(server *Server, agent *coordinator.AgentState) string {
	return server.captureFinishedPane(context.Background(), agent.PaneID, agent.Agent, agent.Cwd, agent.SessionID, projectContextForAgent(agent))
}

func TestCaptureFinishedPaneKimiWithoutAvailableSourceReturnsNoPaneText(t *testing.T) {
	t.Run("relay-chosen source is missing", func(t *testing.T) {
		server, agent, wire := kimiRelayChosenPane(t)
		if err := os.Remove(wire); err != nil {
			t.Fatal(err)
		}
		if got := captureKimiPane(server, agent); got != "" {
			t.Fatalf("capture = %q, want no text when the selected source is missing", got)
		}
	})

	t.Run("relay-chosen source changed", func(t *testing.T) {
		server, agent, wire := kimiRelayChosenPane(t)
		newer := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(wire))), "..", "session_newer")
		if err := os.MkdirAll(filepath.Join(newer, "agents", "main"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(newer, "state.json"), []byte(`{"id":"session_newer","version":2,"cwd":"/fixture","updatedAt":9000}`), 0o600); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(wire)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(newer, "agents", "main", "wire.jsonl"), data, 0o600); err != nil {
			t.Fatal(err)
		}
		if got := captureKimiPane(server, agent); got != "" {
			t.Fatalf("capture = %q, want no text when a newer session replaced the selection", got)
		}
	})

	t.Run("relay-chosen selection is unavailable", func(t *testing.T) {
		server, _, _ := kimiIdentityServer(t)
		server.herdrC = visiblePaneBackend{Backend: server.herdrC}
		unavailable := &coordinator.AgentState{
			PaneID: "second_failure", Agent: "kimi", Cwd: "/fixture", Status: "idle",
			ConversationInferred: true, ConversationReason: "source_unavailable",
		}
		server.state.CommitInventory([]*coordinator.AgentState{unavailable}, server.state.RevisionCounter())
		current, _ := server.state.Agent(unavailable.PaneID)
		if !server.kimiSelectionCurrent(t.Context(), current) {
			t.Fatal("fixture no longer keeps the unavailable selection current, so the read would not reach the source check")
		}
		if got := captureKimiPane(server, current); got != "" {
			t.Fatalf("capture = %q, want no text when the relay has no available source", got)
		}
	})

	t.Run("reported session file is missing", func(t *testing.T) {
		server, reported, wire := kimiReportedPane(t)
		if err := os.Remove(wire); err != nil {
			t.Fatal(err)
		}
		if got := captureKimiPane(server, reported); got != "" {
			t.Fatalf("capture = %q, want no text when the reported session file is missing", got)
		}
	})

	t.Run("reported session file is invalid", func(t *testing.T) {
		server, reported, wire := kimiReportedPane(t)
		if err := os.WriteFile(wire, []byte("not a Kimi wire journal\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := captureKimiPane(server, reported); got != "" {
			t.Fatalf("capture = %q, want no text when the reported session file is invalid", got)
		}
	})
}

func TestCaptureFinishedPaneKimiWithoutAssistantReplyFallsThroughToPaneText(t *testing.T) {
	promptOnly := func(t *testing.T, wire string) {
		t.Helper()
		writeInvariantRows(t, wire, map[string]any{"type": "metadata", "protocol_version": "1.5"}, kimiAppMessage("user", "Seed prompt"))
	}

	t.Run("relay-chosen source", func(t *testing.T) {
		server, agent, wire := kimiRelayChosenPane(t)
		promptOnly(t, wire)
		if got := captureKimiPane(server, agent); got != kimiVisiblePaneText {
			t.Fatalf("capture = %q, want the pane text", got)
		}
	})

	t.Run("reported session", func(t *testing.T) {
		server, reported, wire := kimiReportedPane(t)
		promptOnly(t, wire)
		if got := captureKimiPane(server, reported); got != kimiVisiblePaneText {
			t.Fatalf("capture = %q, want the pane text", got)
		}
	})
}
