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

func TestHandleReadPaneKeepsResizedScrollbackAndFiltersStaleWideRows(t *testing.T) {
	dir := t.TempDir()
	visiblePath := filepath.Join(dir, "visible.ansi")
	recentPath := filepath.Join(dir, "recent.ansi")
	historyLines := make([]string, 120)
	for index := range historyLines {
		historyLines[index] = fmt.Sprintf("history row %03d", index)
	}
	visible := strings.Join(historyLines[len(historyLines)-46:], "\n") + "\n"
	recentLines := append([]string{}, historyLines[:60]...)
	recentLines = append(recentLines, "stale desktop-width row"+strings.Repeat(" ", 20))
	recentLines = append(recentLines, historyLines[60:]...)
	recent := strings.Join(recentLines, "\n") + "\n"
	if err := os.WriteFile(visiblePath, []byte(visible), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recentPath, []byte(recent), 0o600); err != nil {
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
	want := strings.Join(historyLines, "\n") + "\n"
	if response["content"] != want {
		t.Fatalf("pane content has %d lines, want complete %d-line scrollback",
			strings.Count(response["content"].(string), "\n"),
			len(historyLines),
		)
	}
	if response["truncated"] != true {
		t.Fatalf("pane truncation = %#v, want true after filtering", response["truncated"])
	}
}
