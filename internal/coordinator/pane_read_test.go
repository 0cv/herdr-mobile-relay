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

func TestHandleReadPaneUsesOnlyVisibleRowsForResizedPane(t *testing.T) {
	dir := t.TempDir()
	visiblePath := filepath.Join(dir, "visible.ansi")
	recentPath := filepath.Join(dir, "recent.ansi")
	visibleLines := make([]string, 46)
	for index := range visibleLines {
		visibleLines[index] = fmt.Sprintf("visible row %02d", index)
	}
	visibleLines[20] = strings.Repeat("current-streaming-token-", 8)
	visible := strings.Join(visibleLines, "\n") + "\n"
	corruptRecent := strings.Repeat("duplicated desktop redraw\n", 120)
	if err := os.WriteFile(visiblePath, []byte(visible), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recentPath, []byte(corruptRecent), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"case \" $* \" in\n"+
		"  *\" --source visible \"*) cat \""+visiblePath+"\" ;;\n"+
		"  *) cat \""+recentPath+"\" ;;\n"+
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
	if response["content"] != visible {
		t.Fatalf("pane content = %q, want current visible rows", response["content"])
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
}
