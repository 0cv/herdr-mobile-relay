package conversation

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBrowserOMOPlanFromTailKeepsHeadIdentityOfLargeFile(t *testing.T) {
	home := t.TempDir()
	cwd := "/work/project"
	sessionID := "session-large"
	root := filepath.Join(home, ".omo", "agent", "sessions")
	directory := filepath.Join(root, encodeOMOCWD(cwd))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "2026-08-31T12-00-00-000Z_"+sessionID+".jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(file, "{\"type\":\"session\",\"id\":%q,\"cwd\":%q}\n", sessionID, cwd); err != nil {
		t.Fatal(err)
	}
	filler := strings.Repeat("x", 4096) + "\n"
	for written := 0; written <= maxConversationBytes; written += len(filler) {
		if _, err := file.WriteString(filler); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fmt.Fprintf(file, "{\"type\":\"custom\",\"customType\":\"senpi.todo-state\",\"timestamp\":\"2026-08-31T12:01:00Z\",\"data\":{\"schema\":\"v2\",\"phases\":[{\"name\":\"Build\",\"tasks\":[{\"id\":\"one\",\"content\":\"Ship it\",\"status\":\"in_progress\"}]}]}}\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	browser := newTestBrowser(t, NewReader(home), DefaultBrowserOptions())
	scope := BrowseScope{Provider: "omo", CWD: cwd, SessionID: sessionID}
	page, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: scope, Limit: 50})
	if err != nil || !page.Available || page.OMOPlan == nil {
		t.Fatalf("large OMO page = %#v, err = %v", page, err)
	}
	state := *page.OMOPlan
	if !state.Available || len(state.Phases) != 1 || len(state.Phases[0].Tasks) != 1 {
		t.Fatalf("projected OMO state = %#v", state)
	}
	if state.Phases[0].Tasks[0].Content != "Ship it" || !page.Diagnostics.SourceTruncated {
		t.Fatalf("projected task = %#v, diagnostics = %#v", state.Phases[0].Tasks[0], page.Diagnostics)
	}
}

func TestOMOReadFollowsGrowthAndSkipsOversizedRows(t *testing.T) {
	home := t.TempDir()
	cwd := "/work/project"
	sessionID := "session-no-todo"
	root := filepath.Join(home, ".omo", "agent", "sessions")
	directory := filepath.Join(root, encodeOMOCWD(cwd))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "2026-09-02T12-00-00-000Z_"+sessionID+".jsonl")
	if err := os.WriteFile(path, []byte(fmt.Sprintf(
		"{\"type\":\"session\",\"id\":%q,\"cwd\":%q}\n",
		sessionID,
		cwd,
	)), 0o600); err != nil {
		t.Fatal(err)
	}
	reader := NewReader(home)
	page, err := reader.ReadLatest("omo", ProjectContext{CWD: cwd}, sessionID, 50)
	if err != nil || !page.Available || len(page.Entries) != 0 {
		t.Fatalf("OMO page without entries = %#v, err = %v", page, err)
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = fmt.Fprintln(file, strings.Repeat("x", maxOMOTodoBytes+1)); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if _, err = fmt.Fprintln(file, `{"type":"message","message":{"role":"user","content":"after growth"}}`); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	page, err = reader.ReadLatest("omo", ProjectContext{CWD: cwd}, sessionID, 50)
	if err != nil || !page.Available || len(page.Entries) != 1 || page.Entries[0].Text != "after growth" {
		t.Fatalf("OMO page after the file grew = %#v, err = %v", page, err)
	}
}

func TestOMOInvalidOnlyTodoStatePreservesHistory(t *testing.T) {
	home := t.TempDir()
	cwd := "/work/project"
	sessionID := "session-invalid-todo"
	directory := filepath.Join(home, ".omo", "agent", "sessions", encodeOMOCWD(cwd))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "2026-09-02T12-00-00-000Z_"+sessionID+".jsonl")
	transcript := fmt.Sprintf(
		"{\"type\":\"session\",\"id\":%q,\"cwd\":%q}\n"+
			"{\"type\":\"message\",\"message\":{\"role\":\"user\",\"content\":\"keep this\"}}\n"+
			"{\"type\":\"custom\",\"customType\":\"senpi.todo-state\",\"data\":{\"schema\":\"v2\",\"phases\":[{\"name\":\"\",\"tasks\":[]}]}}\n",
		sessionID,
		cwd,
	)
	if err := os.WriteFile(path, []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	reader := NewReader(home)
	page, err := reader.ReadLatest("omo", ProjectContext{CWD: cwd}, sessionID, 50)
	if err != nil {
		t.Fatal(err)
	}
	if !page.Available || len(page.Entries) != 1 || page.Entries[0].Text != "keep this" {
		t.Fatalf("invalid-only OMO page = %#v", page)
	}
}
