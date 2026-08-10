package coordinator

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func TestFilterOverwidePaneRowsPreservesANSIAndTerminalCells(t *testing.T) {
	content := []byte(strings.Join([]string{
		"short",
		"\x1b[31m1234567890EXTRA\x1b[0m",
		"界界界界界Z",
		"界界界界界",
		strings.Repeat("e\u0301", 10) + "X",
		strings.Repeat("e\u0301", 10),
		"\x1b]0;long terminal title\x07ok",
	}, "\r\n") + "\r\n")

	got, filtered := filterOverwidePaneRows(content, 10, true)
	if !filtered {
		t.Fatal("overwide pane rows were not filtered")
	}
	want := []byte(strings.Join([]string{
		"short",
		"界界界界界",
		strings.Repeat("e\u0301", 10),
		"\x1b]0;long terminal title\x07ok",
	}, "\r\n") + "\r\n")
	if !bytes.Equal(got, want) {
		t.Fatalf("filtered pane rows = %q, want %q", got, want)
	}

	unchanged := []byte("one\ntwo\n")
	got, filtered = filterOverwidePaneRows(unchanged, 10, false)
	if filtered || !bytes.Equal(got, unchanged) {
		t.Fatalf("narrow pane rows = %q, filtered = %v", got, filtered)
	}
}

func TestHandleReadPaneUsesOnlyVisibleRowsForResizedPane(t *testing.T) {
	dir := t.TempDir()
	visiblePath := filepath.Join(dir, "visible.ansi")
	recentPath := filepath.Join(dir, "recent.ansi")
	visibleLines := make([]string, 46)
	for index := range visibleLines {
		visibleLines[index] = fmt.Sprintf("visible row %02d", index)
	}
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
		"pane_id": "pane-1", "lines": float64(150), "format": "ansi", "terminal_columns": float64(20),
	})
	if response["content"] != visible {
		t.Fatalf("pane content = %q, want current visible rows", response["content"])
	}
	if response["viewport_only"] != true {
		t.Fatalf("viewport_only = %#v, want true", response["viewport_only"])
	}
	if response["truncated"] != false {
		t.Fatalf("pane truncation = %#v, want false", response["truncated"])
	}
}
