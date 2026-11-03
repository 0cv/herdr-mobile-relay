package app

import (
	"context"
	"slices"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
)

func TestTUIBackendCapabilities(t *testing.T) {
	s := testServer()
	client := herdr.NewClient("/nonexistent/herdr", t.TempDir()+"/missing.sock")
	defer client.Close()
	want := []string{
		"pi_integration", "attention_classification", "clear_activities",
		"directory_browser", "workspace_management", "worktree_management",
		"self_update", "structured_questions", "slash_commands",
		"conversation_history", "conversation_activity", "workspace_inspection",
		"semantic_input", "manual_terminal_input", "secret_input", "invitation_qr",
		"tab_rename", "agent_launch",
	}
	for _, capability := range s.effectiveCapabilitiesFor(herdr.ServerStatus{}) {
		if !slices.Contains(protocol.Capabilities, capability) {
			want = append(want, capability)
		}
	}
	slices.Sort(want)
	assertCapabilities := func(stage string) {
		t.Helper()
		got := s.effectiveCapabilitiesFor(client.CapabilityStatus())
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Fatalf("%s capabilities = %v, want %v", stage, got, want)
		}
	}
	assertCapabilities("construction")
	client.RefreshCapabilities(context.Background())
	assertCapabilities("failed refresh")
	client.InvalidateLiveCapabilities()
	assertCapabilities("invalidation")
}
