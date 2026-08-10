package coordinator

import (
	"bytes"
	"context"
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

func TestHandleReadPaneUsesVisibleRowsAndFiltersActiveLeaseWidth(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "invocations.log")
	panePath := filepath.Join(dir, "pane.ansi")
	pane := "\x1b[32mcorrect row\x1b[0m\n" +
		"narrow text" + strings.Repeat(" ", 9) + "stale desktop-width suffix\n"
	if err := os.WriteFile(panePath, []byte(pane), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"printf '%s\\n' \"$*\" >> \""+record+"\"\n"+
		"cat \""+panePath+"\"\n")
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Agent: "omp", Status: "idle"}}, state.RevisionCounter())
	dispatcher := NewDispatcher(
		herdr.NewClient(bin, filepath.Join(dir, "missing.sock")),
		state,
		nil,
		testLogger(),
	)

	response := dispatcher.HandleReadPane(context.Background(), map[string]any{
		"pane_id": "pane-1", "lines": float64(20), "format": "ansi", "terminal_columns": float64(20),
	})
	if response["content"] != "\x1b[32mcorrect row\x1b[0m\n" {
		t.Fatalf("pane content = %q", response["content"])
	}
	if response["truncated"] != true {
		t.Fatalf("pane truncation = %#v, want true after filtering", response["truncated"])
	}
	invocations, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(invocations), "--source visible --format ansi") {
		t.Fatalf("pane read did not request visible rows: %s", invocations)
	}
	if strings.Contains(string(invocations), "recent-unwrapped") || strings.Contains(string(invocations), "--source recent ") {
		t.Fatalf("pane read requested scrollback rows during a size lease: %s", invocations)
	}
}
