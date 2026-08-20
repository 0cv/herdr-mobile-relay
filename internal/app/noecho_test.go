package app

import (
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
)

// The phone locks its generic composer on a pane the attention classifier
// cannot read, so the masked secret input is unlocked by this flag alone. It
// must therefore ride on the frame the phone renders — the history-merged one.
func TestPreparePaneResponseFlagsNoEchoPromptOnMergedContent(t *testing.T) {
	s := testServerWithCacheDir(t.TempDir())
	s.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", Agent: "claude", Status: "idle",
	}}, s.state.RevisionCounter())
	s.historyM.Merge(
		"pane-1",
		"history 1\nhistory 2\nhistory 3\nhistory 4\nhistory 5\nhistory 6\nhistory 7\nhistory 8",
	)

	response := map[string]any{
		"type": "pane_content", "pane_id": "pane-1", "format": "ansi",
		"content": "$ sudo dnf upgrade\n[sudo] password for cv: ",
	}
	s.preparePaneResponse(map[string]any{"pane_id": "pane-1", "lines": 100}, response)

	merged, ok := response["content"].(string)
	if !ok || !strings.HasSuffix(strings.TrimSpace(merged), "[sudo] password for cv:") {
		t.Fatalf("merged content does not end at the sudo prompt: %q", response["content"])
	}
	if response["no_echo"] != true {
		t.Fatalf("no_echo = %#v, want true", response["no_echo"])
	}
	if response["no_echo_prompt"] != "[sudo] password for cv:" {
		t.Fatalf("no_echo_prompt = %#v", response["no_echo_prompt"])
	}
}

func TestPreparePaneResponseMarksOrdinaryFramesAsEchoing(t *testing.T) {
	s := testServerWithCacheDir(t.TempDir())
	s.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-2", Agent: "omp", Status: "idle",
	}}, s.state.RevisionCounter())

	response := map[string]any{
		"type": "pane_content", "pane_id": "pane-2", "format": "ansi",
		"content": "Reset the stored password y/n\n❯ ",
	}
	s.preparePaneResponse(map[string]any{"pane_id": "pane-2", "lines": 100}, response)

	if response["no_echo"] != false {
		t.Fatalf("no_echo = %#v, want false", response["no_echo"])
	}
	if _, exists := response["no_echo_prompt"]; exists {
		t.Fatalf("echoing frame carried a no-echo prompt: %#v", response["no_echo_prompt"])
	}
}

// A pane_delta reuses every metadata field of the frame it derives from, so the
// masked input survives a delta update.
func TestPaneDeltaCarriesNoEchoMetadata(t *testing.T) {
	response := map[string]any{
		"type": "pane_content", "pane_id": "pane-1", "content": "[sudo] password for cv:",
		"no_echo": true, "no_echo_prompt": "[sudo] password for cv:",
	}
	acknowledged := &paneWatchFrame{
		content:            "[sudo] password for cv:",
		contentFingerprint: "content-1",
		frameFingerprint:   "frame-1",
	}
	current := &paneWatchFrame{
		content:            "[sudo] password for cv:",
		contentFingerprint: "content-1",
		frameFingerprint:   "frame-2",
	}

	update := paneWatchUpdate(response, acknowledged, current)
	if update["type"] != "pane_delta" {
		t.Fatalf("update = %#v, want a pane_delta", update)
	}
	if update["no_echo"] != true || update["no_echo_prompt"] != "[sudo] password for cv:" {
		t.Fatalf("delta lost the no-echo metadata: %#v", update)
	}
}

// A secret is a pane write: it must be serialized with the other pane
// mutations rather than admitted ahead of them.
func TestSecretInputIsAnOrderedMutation(t *testing.T) {
	if !isCoordinatorMutation("send_secret") {
		t.Fatal("send_secret is not ordered with the other pane mutations")
	}
}
