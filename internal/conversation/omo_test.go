package conversation

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOMOTodoProjectionKeepsHeadIdentityWhenTranscriptTailIsClipped(t *testing.T) {
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

	state := NewReader(home).ReadOMOTodoState(cwd, sessionID)
	if !state.Available || !state.Truncated || len(state.Phases) != 1 || len(state.Phases[0].Tasks) != 1 {
		t.Fatalf("projected OMO state = %#v", state)
	}
	if state.Phases[0].Tasks[0].Content != "Ship it" {
		t.Fatalf("projected task = %#v", state.Phases[0].Tasks[0])
	}
}

func TestOMOWithoutTodoIsAvailableAndOversizedRowsAreSkipped(t *testing.T) {
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
	page, err := reader.readOMO(cwd, sessionID, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if page.OMOPlan == nil || !page.OMOPlan.Available || page.OMOPlan.ReasonCode != "" {
		t.Fatalf("normal no-todo plan = %#v", page.OMOPlan)
	}
	reader.mu.Lock()
	cacheEntries := len(reader.omoCache)
	reader.mu.Unlock()
	if cacheEntries != 1 {
		t.Fatalf("OMO transcript cache entries = %d", cacheEntries)
	}

	valid := `{"type":"custom","customType":"senpi.todo-state","timestamp":"2026-09-02T12:01:00Z","data":{"schema":"v2","phases":[{"name":"Build","tasks":[{"content":"Ship","status":"pending"}]}]}}`
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = fmt.Fprintln(file, valid); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	state := reader.ReadOMOTodoState(cwd, sessionID)
	if !state.Available || len(state.Phases) != 1 || state.Phases[0].Tasks[0].Content != "Ship" {
		t.Fatalf("state after cache invalidation = %#v", state)
	}
	state, found, invalidOnly := latestValidOMOTodo(strings.Repeat("x", maxOMOTodoBytes+1)+"\n"+valid, sessionID)
	if !found || invalidOnly || len(state.Phases) != 1 || state.Phases[0].Tasks[0].Content != "Ship" {
		t.Fatalf("state after oversized row = %#v, found %v, invalid only %v", state, found, invalidOnly)
	}
}

func TestOMOInvalidOnlyTodoStateReportsCorruptSource(t *testing.T) {
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
			"{\"type\":\"custom\",\"customType\":\"senpi.todo-state\",\"data\":{\"schema\":\"v2\",\"phases\":[{\"name\":\"\",\"tasks\":[]}]}}\n",
		sessionID,
		cwd,
	)
	if err := os.WriteFile(path, []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	reader := NewReader(home)
	page, err := reader.readOMO(cwd, sessionID, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if page.Available || page.ReasonCode != "source_corrupt" {
		t.Fatalf("invalid-only OMO page = %#v", page)
	}
	reader.mu.Lock()
	cacheEntries := len(reader.omoCache)
	reader.mu.Unlock()
	if cacheEntries != 0 {
		t.Fatalf("invalid OMO source cached %d entries", cacheEntries)
	}
}
