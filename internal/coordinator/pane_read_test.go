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

func TestHandleReadPaneKeepsFullHistoryForResizedPane(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "invocations.log")
	visiblePath := filepath.Join(dir, "visible.ansi")
	historyPath := filepath.Join(dir, "history.ansi")
	visibleLines := make([]string, 46)
	for index := range visibleLines {
		visibleLines[index] = fmt.Sprintf("visible row %02d", index)
	}
	historyLines := make([]string, 120)
	for index := range historyLines {
		historyLines[index] = fmt.Sprintf("history row %03d", index)
	}
	visible := strings.Join(visibleLines, "\n") + "\n"
	historyContent := strings.Join(historyLines, "\n") + "\n"
	if err := os.WriteFile(visiblePath, []byte(visible), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(historyPath, []byte(historyContent), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"printf '%s\\n' \"$*\" >> \""+record+"\"\n"+
		"case \" $* \" in\n"+
		"  *\" --source visible \"*) cat \""+visiblePath+"\" ;;\n"+
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
	if response["content"] != historyContent {
		t.Fatalf("pane content has %d lines, want complete %d-line history",
			strings.Count(response["content"].(string), "\n"),
			len(historyLines),
		)
	}
	if _, ok := response["viewport_only"]; ok {
		t.Fatalf("viewport_only = %#v, want full history response", response["viewport_only"])
	}
	if response["truncated"] != false {
		t.Fatalf("pane truncation = %#v, want false", response["truncated"])
	}
	invocations, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(invocations), "--source recent-unwrapped --format ansi") {
		t.Fatalf("pane read did not request full unwrapped history: %s", invocations)
	}
}
