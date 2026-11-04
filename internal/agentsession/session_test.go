package agentsession

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestSession(t *testing.T) {
	for _, tt := range []struct{ name, agent, body, key, value, want string }{
		{"agy", "agy", `{"conversationId":"abc","session_id":"wrong"}`, "", "", "abc"},
		{"agy missing", "agy", `{"session_id":"abc"}`, "", "", ""},
		{"agy wrong type", "agy", `{"conversationId":123}`, "", "", ""},
		{"claude", "claude", `{"session_id":"abc","hook_event_name":"SessionStart"}`, "", "", "abc"},
		{"claude exact", "claude", `{"session_id":" abc ","hook_event_name":"SessionStart"}`, "", "", " abc "},
		{"subagent", "claude", `{"session_id":"abc","hook_event_name":"SessionStart","agent_id":"sub"}`, "", "", ""},
		{"cursor payload", "claude", `{"session_id":"abc","hook_event_name":"SessionStart","cursor_version":""}`, "", "", ""},
		{"cursor env", "claude", `{"session_id":"abc","hook_event_name":"SessionStart"}`, "CURSOR_VERSION", "1", ""},
		{"claude event", "claude", `{"session_id":"abc"}`, "", "", ""},
		{"codex", "codex", `{"session_id":"abc","transcript_path":"/tmp/log"}`, "", "", "abc"},
		{"codex event", "codex", `{"session_id":"abc","transcript_path":"/tmp/log","hook_event_name":"SessionStart"}`, "CODEX_THREAD_ID", "abc", "abc"},
		{"codex inherited", "codex", `{"session_id":"abc","transcript_path":"/tmp/log"}`, "CODEX_THREAD_ID", "other", ""},
		{"codex transcript", "codex", `{"session_id":"abc","transcript_path":" "}`, "", "", ""},
		{"codex other event", "codex", `{"session_id":"abc","transcript_path":"/tmp/log","hook_event_name":"Stop"}`, "", "", ""},
		{"invalid", "claude", `{`, "", "", ""},
		{"wrong type", "codex", `{"session_id":123,"transcript_path":"/tmp/log"}`, "", "", ""},
		{"unknown", "pi", `{"session_id":"abc"}`, "", "", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := session(tt.agent, []byte(tt.body), func(key string) string {
				if key == tt.key {
					return tt.value
				}
				return ""
			})
			if got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestAncestor(t *testing.T) {
	parent := func(_ context.Context, pid int) (int, error) { return pid - 1, nil }
	for _, tt := range []struct {
		target, child int
		want          bool
	}{{8, 10, true}, {10, 10, false}, {11, 10, false}, {1, 10, false}, {2, 100, false}} {
		if got := ancestor(t.Context(), tt.target, tt.child, parent); got != tt.want {
			t.Fatalf("%+v: %v", tt, got)
		}
	}
	if ancestor(t.Context(), 8, 10, func(context.Context, int) (int, error) { return 0, errors.New("gone") }) {
		t.Fatal("accepted error")
	}
	if ancestor(t.Context(), 8, 10, func(context.Context, int) (int, error) { return 10, nil }) {
		t.Fatal("accepted cycle")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if ancestor(ctx, 8, 10, parent) {
		t.Fatal("accepted cancellation")
	}
	if !ancestor(t.Context(), os.Getppid(), os.Getpid(), parentPID) {
		t.Fatal("ps did not find actual parent")
	}
}

type unreadable struct{}

func (unreadable) Read([]byte) (int, error) { panic("stdin read outside core") }

func TestOutsideCore(t *testing.T) {
	for _, pair := range [][2]string{{"", ""}, {"t1", ""}, {"", "/not-used"}} {
		t.Setenv("HC_TERMINAL_ID", pair[0])
		t.Setenv("HC_TERMINAL_SOCKET", pair[1])
		Run("claude", unreadable{})
	}
}

func TestBoundedInput(t *testing.T) {
	t.Setenv("HC_TERMINAL_ID", "t1")
	t.Setenv("HC_TERMINAL_SOCKET", t.TempDir()+"/missing")
	Run("claude", strings.NewReader(strings.Repeat("x", maxPayload+1)))
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	start := time.Now()
	Run("claude", reader)
	if time.Since(start) > 3*time.Second {
		t.Fatal("input exceeded deadline")
	}
}
