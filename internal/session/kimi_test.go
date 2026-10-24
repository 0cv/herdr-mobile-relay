package session

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
	"github.com/0cv/herdr-mobile-relay/internal/conversation"
)

func TestKimiTitleAndTranscriptShareSelectedRoot(t *testing.T) {
	home, first, second := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv(agentroots.KimiCodeListEnv, first+string(os.PathListSeparator)+second)
	t.Setenv("KIMI_CODE_HOME", "")
	cwd, id := "/fixture", "session_title"
	hash := sha256.Sum256([]byte(cwd))
	for i, root := range []string{first, second} {
		dir := filepath.Join(root, "sessions", fmt.Sprintf("wd_fixture_%x", hash[:6]), id)
		if err := os.MkdirAll(filepath.Join(dir, "agents", "main"), 0o700); err != nil {
			t.Fatal(err)
		}
		state, _ := json.Marshal(map[string]any{"id": id, "version": 2, "cwd": cwd, "updatedAt": 1, "title": fmt.Sprintf("Title %d", i)})
		if err := os.WriteFile(filepath.Join(dir, "state.json"), state, 0o600); err != nil {
			t.Fatal(err)
		}
		wire := fmt.Sprintf("{\"type\":\"metadata\",\"protocol_version\":\"1.5\"}\n{\"type\":\"context.append_message\",\"message\":{\"role\":\"user\",\"content\":[{\"type\":\"text\",\"text\":\"Root %d\"}]}}\n", i)
		if err := os.WriteFile(filepath.Join(dir, "agents", "main", "wire.jsonl"), []byte(wire), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reader := conversation.NewReader(home)
	resolver := NewResolverWithReader(home, reader)
	if title := resolver.SessionName("kimi", cwd, id); title != "Title 0" {
		t.Fatal(title)
	}
	page, err := reader.ReadFor("kimi", cwd, id, "", 1)
	if err != nil || len(page.Entries) != 1 || page.Entries[0].Text != "Root 0" {
		t.Fatalf("page=%+v error=%v", page, err)
	}
	if err := os.RemoveAll(filepath.Join(first, "sessions")); err != nil {
		t.Fatal(err)
	}
	if title := resolver.SessionName("Kimi-Code", cwd, id); title != "Title 1" {
		t.Fatal(title)
	}
	page, err = reader.ReadFor("kimi", cwd, id, "", 1)
	if err != nil || len(page.Entries) != 1 || page.Entries[0].Text != "Root 1" {
		t.Fatalf("page=%+v error=%v", page, err)
	}
}
