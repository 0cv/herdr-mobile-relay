package app

import (
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
)

func TestPreparePaneResponsePreservesHistoryForViewportOnlyRead(t *testing.T) {
	s := testServerWithCacheDir(t.TempDir())
	s.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", Agent: "claude", Status: "idle",
	}}, s.state.RevisionCounter())
	baseline := "history 1\nhistory 2\nhistory 3\nhistory 4\nhistory 5\nhistory 6\nhistory 7\nhistory 8"
	s.historyM.Merge("pane-1", baseline)

	response := map[string]any{
		"type": "pane_content", "pane_id": "pane-1",
		"content": "current 1\ncurrent 2", "format": "ansi",
		"truncated": false, "viewport_only": true,
	}
	s.preparePaneResponse(
		coordinator.PaneReadRequest{PaneID: "pane-1", Lines: 100, Columns: 59},
		response,
	)

	if response["content"] != "current 1\ncurrent 2" {
		t.Fatalf("resized content = %q, want clean current viewport", response["content"])
	}
	if response["truncated"] != false {
		t.Fatalf("resized truncation = %#v, want false", response["truncated"])
	}
	if content := s.historyM.Content("pane-1", 100); content != baseline {
		t.Fatalf("history changed during resized read:\n%s", content)
	}
}

func TestUnchangedPaneResponseSuppressesTerminalContent(t *testing.T) {
	response := map[string]any{
		"request_id": "identified-read",
		"type":       "pane_content",
		"pane_id":    "w1:p1",
		"content":    "unchanged output",
		"format":     "ansi",
		"target": protocol.TargetRef{
			ServerSessionID: "primary",
			PaneID:          "w1:p1",
			TerminalID:      "terminal-w1:p1",
			Generation:      4,
		},
	}
	fingerprint := paneFingerprint("unchanged output")
	unchanged := unchangedPaneResponse(
		fingerprint,
		response,
	)
	if unchanged == nil {
		t.Fatal("matching terminal content was not suppressed")
	}
	if unchanged["type"] != "pane_unchanged" || unchanged["pane_id"] != "w1:p1" {
		t.Fatalf("unexpected unchanged response: %#v", unchanged)
	}
	if unchanged["target"] != response["target"] {
		t.Fatalf("unchanged response lost exact target: %#v", unchanged)
	}
	if unchanged["request_id"] != response["request_id"] {
		t.Fatalf("unchanged response lost read identity: %#v", unchanged)
	}
	if _, included := unchanged["content"]; included {
		t.Fatalf("unchanged response included terminal content: %#v", unchanged)
	}
	if response["content_fingerprint"] != fingerprint {
		t.Fatalf("full response fingerprint = %v, want %s", response["content_fingerprint"], fingerprint)
	}

	changed := unchangedPaneResponse(
		"older",
		response,
	)
	if changed != nil {
		t.Fatalf("changed terminal content was suppressed: %#v", changed)
	}
}
