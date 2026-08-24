package coordinator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func TestHandleReadPaneUsesRecentRowsForResizedPane(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "invocations.log")
	recentPath := filepath.Join(dir, "recent.ansi")
	historyPath := filepath.Join(dir, "history.ansi")
	recentLines := make([]string, 120)
	for index := range recentLines {
		recentLines[index] = fmt.Sprintf("recent row %03d", index)
	}
	historyLines := make([]string, 120)
	for index := range historyLines {
		historyLines[index] = fmt.Sprintf("history row %03d", index)
	}
	recent := strings.Join(recentLines, "\n") + "\n"
	historyContent := strings.Join(historyLines, "\n") + "\n"
	if err := os.WriteFile(recentPath, []byte(recent), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(historyPath, []byte(historyContent), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"printf '%s\\n' \"$*\" >> \""+record+"\"\n"+
		"case \" $* \" in\n"+
		"  *\" --source recent \"*) cat \""+recentPath+"\" ;;\n"+
		"  *) cat \""+historyPath+"\" ;;\n"+
		"esac\n")
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Agent: "omp", Status: "idle"}}, state.RevisionCounter())
	dispatcher := NewDispatcher(
		herdr.NewClient(bin, filepath.Join(dir, "missing.sock")),
		state,
		nil,
		testLogger(),
	)

	response := dispatcher.HandleReadPane(context.Background(), map[string]any{
		"pane_id": "pane-1", "lines": float64(150), "format": "ansi",
		"terminal_columns": float64(20), "terminal_rows": float64(46),
	})
	if response["content"] != recent {
		t.Fatalf("pane content has %d lines, want the %d-line recent window",
			strings.Count(response["content"].(string), "\n"),
			len(recentLines),
		)
	}
	if response["viewport_only"] != true {
		t.Fatalf("viewport_only = %#v, want true", response["viewport_only"])
	}
	if response["viewport_rows"] != 46 {
		t.Fatalf("viewport_rows = %#v, want 46", response["viewport_rows"])
	}
	if response["truncated"] != false {
		t.Fatalf("pane truncation = %#v, want false", response["truncated"])
	}
	invocations, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(invocations), "--source recent --format ansi") {
		t.Fatalf("pane read did not request the recent resized window: %s", invocations)
	}
}

// Display reads serve physical rows so the Resize Session baseline shares row
// semantics with resized frames; only Claude keeps logical lines for its
// alternate-screen history merge. Both apply to ansi reads, the only format
// the app asks for.
func TestHandleReadPaneSourceFollowsAgent(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "invocations.log")
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"printf '%s\\n' \"$*\" >> \""+record+"\"\n"+
		"echo content\n")
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{
		{PaneID: "pane-omp", Agent: "omp", Status: "idle"},
		{PaneID: "pane-claude", Agent: "claude", Status: "idle"},
	}, state.RevisionCounter())
	dispatcher := NewDispatcher(
		herdr.NewClient(bin, filepath.Join(dir, "missing.sock")),
		state,
		nil,
		testLogger(),
	)

	for _, paneID := range []string{"pane-omp", "pane-claude"} {
		response := dispatcher.HandleReadPane(context.Background(), map[string]any{
			"pane_id": paneID, "lines": float64(100), "format": "ansi",
		})
		if _, failed := response["error"]; failed {
			t.Fatalf("read %s failed: %#v", paneID, response)
		}
	}
	invocations, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(invocations), "pane-omp --lines 100 --source recent ") {
		t.Fatalf("omp read did not request physical rows: %s", invocations)
	}
	if !strings.Contains(string(invocations), "pane-claude --lines 100 --source recent-unwrapped ") {
		t.Fatalf("claude read did not request logical lines: %s", invocations)
	}
}

// A "recent"/"recent-unwrapped" read in text format above the pane's viewport
// height makes Herdr harvest scrollback through the agent's mouse-scroll
// interface, which visibly scrolls the operator's real pane. No display read
// may reach that path.
func TestTextFormatDisplayReadNeverHarvestsScrollback(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "invocations.log")
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"printf '%s\\n' \"$*\" >> \""+record+"\"\n"+
		"echo content\n")
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{
		{PaneID: "pane-omp", Agent: "omp", Status: "idle"},
		{PaneID: "pane-claude", Agent: "claude", Status: "idle"},
	}, state.RevisionCounter())
	dispatcher := NewDispatcher(
		herdr.NewClient(bin, filepath.Join(dir, "missing.sock")),
		state,
		nil,
		testLogger(),
	)

	for _, paneID := range []string{"pane-omp", "pane-claude"} {
		for _, columns := range []float64{0, 59} {
			response := dispatcher.HandleReadPane(context.Background(), map[string]any{
				"pane_id": paneID, "lines": float64(400), "format": "text",
				"terminal_columns": columns,
			})
			if _, failed := response["error"]; failed {
				t.Fatalf("read %s failed: %#v", paneID, response)
			}
		}
	}
	invocations, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(invocations), "--source recent") {
		t.Fatalf("a text-format read asked for harvested scrollback: %s", invocations)
	}
	if strings.Count(string(invocations), "--source visible --format text") != 4 {
		t.Fatalf("text-format reads did not all use the visible screen: %s", invocations)
	}
}

// The seeding path is the one caller allowed to harvest, and it must ask for the
// logical transcript in text format: an ansi read of an alternate-screen pane
// never returns more than the visible screen.
func TestHarvestPaneTranscriptReadsUnwrappedText(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "invocations.log")
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"printf '%s\\n' \"$*\" >> \""+record+"\"\n"+
		"echo harvested\n")
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{
		{PaneID: "pane-claude", Agent: "claude", Status: "idle"},
	}, state.RevisionCounter())
	dispatcher := NewDispatcher(
		herdr.NewClient(bin, filepath.Join(dir, "missing.sock")),
		state,
		nil,
		testLogger(),
	)

	content, _, err := dispatcher.HarvestPaneTranscript(context.Background(), "pane-claude", 1000)
	if err != nil {
		t.Fatalf("harvest failed: %v", err)
	}
	if strings.TrimSpace(content) != "harvested" {
		t.Fatalf("harvested content = %q", content)
	}
	invocations, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(invocations), "--lines 1000 --source recent-unwrapped --format text") {
		t.Fatalf("harvest did not request the logical transcript: %s", invocations)
	}
}
