package agentsession

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMoreAgentPayloads(t *testing.T) {
	for _, tc := range []struct{ agent, data, want string }{
		{"cursor", `{"hook_event_name":"sessionStart","session_id":"s"}`, "s"},
		{"cursor", `{"conversation_id":"c"}`, "c"},
		{"cursor", `{"hookEventName":"sessionStart","conversationId":"c"}`, "c"},
		{"cursor", `{"hook_event_name":"stop","session_id":"s"}`, ""},
		{"cursor", `{"session_id":4}`, ""},
		{"omp", `{"session_id":"/sessions/s.jsonl"}`, "/sessions/s.jsonl"},
		{"omp", `{"session_id":null}`, ""},
		{"qoder", `{"session_id":"s","hook_event_name":"SessionStart","source":"startup"}`, "s"},
		{"qoder", `{"session_id":"s","hook_event_name":"SessionStart","source":"resume"}`, "s"},
		{"qoder", `{"session_id":"s","hook_event_name":"SessionStart","agent_id":"sub"}`, ""},
		{"qoder", `{"session_id":"s","hook_event_name":"SessionStart","agent_id":null}`, ""},
		{"qoder", `{"session_id":"s","hook_event_name":"Stop"}`, ""},
		{"qoder", `{"session_id":3,"hook_event_name":"SessionStart"}`, ""},
		{"qoder", `{`, ""},
	} {
		if got := session(tc.agent, []byte(tc.data), func(string) string { return "" }); got != tc.want {
			t.Fatalf("%s: %q", tc.data, got)
		}
	}
}

func TestOmpExtensionLifecycle(t *testing.T) {
	home := t.TempDir()
	var out bytes.Buffer
	run := func(op, binary string) {
		t.Helper()
		if err := manageOmpExtension(home, binary, op, &out); err != nil {
			t.Fatal(err)
		}
	}
	run("install", "/relay")
	if _, err := os.Stat(filepath.Join(home, ".omp")); !os.IsNotExist(err) {
		t.Fatal("created absent config")
	}
	dir := filepath.Join(home, ".omp", "agent", "extensions")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	herdr := filepath.Join(dir, "herdr-omp-agent-state.ts")
	if err := os.WriteFile(herdr, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "herdr-mobile-relay-session.ts")
	binary := "/relay's \"binary\""
	run("install", binary)
	data := fileBytes(t, path)
	for _, want := range []string{string(raw(binary)), "session_start", "session_switch", "ctx.hasUI", "process.env.OMPCODE", "process.env.HC_TERMINAL_SOCKET", "getSessionFile() || ctx.sessionManager.getSessionId()", "child.on(\"error\"", "child.stdin.on(\"error\"", "catch {}", "JSON.stringify({ session_id })"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("missing %s", want)
		}
	}
	run("install", binary)
	if !bytes.Equal(data, fileBytes(t, path)) {
		t.Fatal("not idempotent")
	}
	run("install", "/new")
	if bytes.Equal(data, fileBytes(t, path)) {
		t.Fatal("not updated")
	}
	out.Reset()
	run("status", "")
	if out.String() != "omp: installed\n" {
		t.Fatal(out.String())
	}
	run("uninstall", "")
	run("uninstall", "")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("not removed")
	}
	if string(fileBytes(t, herdr)) != "preserve" {
		t.Fatal("Herdr changed")
	}
}
