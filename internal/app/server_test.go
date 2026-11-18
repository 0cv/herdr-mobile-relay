package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/deviceauth"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/session"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

func testServer() *Server {
	return testServerWithCacheDir("")
}

func testServerWithCacheDir(cacheDir string) *Server {
	cfg := &config.Config{
		Host:       "127.0.0.1",
		Port:       8375,
		InstanceID: "test-instance",
		CacheDir:   cacheDir,
	}
	return New(cfg, "0.9.0", "abc123", slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func isAuditedWrite(action string) bool {
	metadata, known := protocol.ClassifyAction(action)
	return known && metadata.Audited
}

func isCoordinatorMutation(action string) bool {
	metadata, known := protocol.ClassifyAction(action)
	return known && metadata.Coordinated
}

func (s *Server) committedAgents() []*coordinator.AgentState {
	return s.committedInventorySnapshot().agents
}

func (s *Server) committedInventoryStatus() map[string]any {
	return s.committedInventorySnapshot().status
}

func publishInventoryForTest(t *testing.T, server *Server) {
	t.Helper()
	if err := server.publishCurrentInventory(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestResolveAgentSessionName(t *testing.T) {
	home := t.TempDir()
	codexDir := filepath.Join(home, ".codex")
	const codexSessionID = "123e4567-e89b-12d3-a456-426614174001"
	rolloutPath := filepath.Join(codexDir, "sessions", "2026", "08", "12", "rollout-2026-08-12T10-00-00-"+codexSessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(rolloutPath), 0o755); err != nil {
		t.Fatal(err)
	}
	row, err := json.Marshal(map[string]any{
		"timestamp": "2026-08-12T10:00:00Z", "type": "response_item",
		"payload": map[string]any{"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": "build it"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rolloutPath, append(row, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	index := []byte("{\"id\":\"" + codexSessionID + "\",\"thread_name\":\"current-session\"}\n")
	if err := os.WriteFile(filepath.Join(codexDir, "session_index.jsonl"), index, 0o644); err != nil {
		t.Fatal(err)
	}

	s := testServer()
	s.sessions = session.NewResolverWithReader(home, nil)

	// named carries a canonical session id with both an index record and the
	// rollout the reader would serve, the shape Locate requires post round-3.
	named := &coordinator.AgentState{Agent: "codex", Session: codexSessionID}
	s.resolveAgentSessionName(named)
	if named.SessionName != "current-session" || named.Session != "current-session" ||
		named.SessionID != codexSessionID || !named.ConversationHistoryAvailable {
		t.Fatalf("named session = %#v, want resolved display name and retained history identity", named)
	}

	unnamed := &coordinator.AgentState{Agent: "codex", Session: "missing-session"}
	s.resolveAgentSessionName(unnamed)
	if unnamed.SessionName != "" || unnamed.Session != "missing-session" ||
		unnamed.SessionID != "missing-session" || !unnamed.ConversationHistoryAvailable {
		t.Fatalf("unnamed session = %#v, want preserved history identity", unnamed)
	}
}

// A whitespace-only session value is what herdr reports for a pane with no
// real session yet - not the same as an empty string, but every other
// consumer (Reader.ReadLatest, latestConversationResponse, the activity backfill
// path) treats it as one via TrimSpace. Before the fix,
// resolveAgentSessionName did not, so the untrimmed value survived the
// `== ""` guard and reached the resolver's empty-id sole-transcript
// heuristic, which - given a cwd whose project directory holds exactly one
// transcript - invents a title for a session that was never reported. The
// conversation view for the same (agent, cwd) reads its session id through
// Reader.ReadLatest, which trims it back to empty and answers "not available", so
// the pane would show a confident title next to an unavailable transcript.
func TestResolveAgentSessionNameTrimsWhitespaceOnlySession(t *testing.T) {
	home := t.TempDir()
	const cwd = "/work/app"
	writeClaudeTranscript(t, filepath.Join(home, ".claude", "projects", "-work-app", invariantSession+".jsonl"), "Sole Transcript Title")

	s := testServer()
	s.sessions = session.NewResolverWithReader(home, nil)

	agent := &coordinator.AgentState{Agent: "claude", Cwd: cwd, Session: "   "}
	s.resolveAgentSessionName(agent)
	if agent.SessionName != "" {
		t.Fatalf("SessionName = %q, want empty for a whitespace-only session", agent.SessionName)
	}
	if agent.SessionID != "" {
		t.Fatalf("SessionID = %q, want empty for a whitespace-only session", agent.SessionID)
	}
	if agent.ConversationHistoryAvailable {
		t.Fatal("ConversationHistoryAvailable = true, want false for a whitespace-only session")
	}
}

func TestForegroundCwdIsNotSerializedInAgentPayload(t *testing.T) {
	agent := &coordinator.AgentState{Agent: "claude", Cwd: "/work/pane", ForegroundCwd: "/work/foreground"}
	data, err := json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["cwd"] != agent.Cwd || strings.Contains(string(data), "foreground_cwd") || strings.Contains(string(data), agent.ForegroundCwd) {
		t.Fatalf("serialized agent payload = %s, want pane cwd only", data)
	}
}

func enrollBootstrapDevice(t *testing.T, runtimeDir, token string) {
	t.Helper()
	store, err := deviceauth.Open(filepath.Join(runtimeDir, "device-auth"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureBootstrapInvitation([]byte(token), "relay", "en"); err != nil {
		t.Fatal(err)
	}
	result, err := store.CompleteE2EEAuth(context.Background(), transport.E2EEAuthSelector{
		Kind: transport.E2EEAuthInvitation, ID: "bootstrap", Version: 1, Locale: "en",
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteE2EEAuth(context.Background(), transport.E2EEAuthSelector{
		Kind: transport.E2EEAuthCredential, ID: result.Identity.CredentialID,
		Version: result.Identity.CredentialVersion, Locale: result.Identity.Locale,
	}, true); err != nil {
		t.Fatal(err)
	}
}

func TestRearmedLaunchStartsWithoutStrandedDevices(t *testing.T) {
	token := strings.Repeat("k", 32)
	for _, test := range []struct {
		name    string
		rearm   bool
		devices int
	}{
		{name: "quick tunnel forgets devices stranded under the previous hostname", rearm: true, devices: 0},
		{name: "stable install keeps its paired devices", rearm: false, devices: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			runtimeDir := filepath.Join(root, "runtime")
			enrollBootstrapDevice(t, runtimeDir, token)
			cfg := &config.Config{
				Token:          token,
				RearmBootstrap: test.rearm,
				RuntimeDir:     runtimeDir,
				CacheDir:       filepath.Join(root, "cache"),
			}
			s := New(cfg, "0.9.0", "abc123", slog.New(slog.NewTextHandler(io.Discard, nil)))
			if s.deviceAuth == nil {
				t.Fatal("device store was not initialized")
			}
			if got := len(s.deviceAuth.ListCredentials("")); got != test.devices {
				t.Fatalf("paired devices after launch = %d, want %d", got, test.devices)
			}
		})
	}
}

func TestServerRunAndShutdown(t *testing.T) {
	root := t.TempDir()
	webRoot := filepath.Join(root, "web")
	if err := os.MkdirAll(webRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(webRoot, "index.html"), []byte("<html></html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Host:        "127.0.0.1",
		Port:        18999,
		InstanceID:  "shutdown-test",
		WebRoot:     webRoot,
		RuntimeDir:  filepath.Join(root, "runtime"),
		CacheDir:    filepath.Join(root, "cache"),
		ConfigHome:  filepath.Join(root, "config"),
		ReleaseRoot: filepath.Join(root, "release"),
	}
	s := New(cfg, "0.9.0", "abc123", slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// One second of polling is not a startup budget on a loaded CI runner under
	// -race, and a server that died immediately should say so instead of timing
	// out with a connection refused.
	deadline := time.Now().Add(15 * time.Second)
	var resp *http.Response
	var err error
	for time.Now().Before(deadline) {
		select {
		case runErr := <-done:
			t.Fatalf("Run returned before the server answered: %v", runErr)
		default:
		}
		if resp, err = http.Get("http://127.0.0.1:18999/health"); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("server did not answer /health within 15s: %v", err)
	}
	resp.Body.Close()

	pid, err := os.ReadFile(filepath.Join(cfg.RuntimeDir, "relay.pid"))
	if err != nil || strings.TrimSpace(string(pid)) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("relay.pid = %q, %v; want this process id", pid, err)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.RuntimeDir, "relay.pid")); !os.IsNotExist(err) {
		t.Fatalf("relay.pid survives shutdown: %v", err)
	}
}
