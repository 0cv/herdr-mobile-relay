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
