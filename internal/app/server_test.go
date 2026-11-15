package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/activity"
	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/deviceauth"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/push"
	"github.com/0cv/herdr-mobile-relay/internal/question"
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
// consumer (Reader.Read, latestConversationResponse, the activity backfill
// path) treats it as one via TrimSpace. Before the fix,
// resolveAgentSessionName did not, so the untrimmed value survived the
// `== ""` guard and reached the resolver's empty-id sole-transcript
// heuristic, which - given a cwd whose project directory holds exactly one
// transcript - invents a title for a session that was never reported. The
// conversation view for the same (agent, cwd) reads its session id through
// Reader.Read, which trims it back to empty and answers "not available", so
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

func TestClassifyBlockedTransitionRetriesPartialCodexQuestion(t *testing.T) {
	partial := `
Question 1/3 (3 unanswered)
After a respondent finishes a questionnaire, what should the app do?

› 1. Builder-defined result (Recommended)  Show the configured result.
  2. Response dashboard                    Store responses for review.
  3. Personalized output                   Generate a tailored result.
  4. None of the above                     Optionally, add details in notes (tab).
`
	complete := partial + `
tab to add notes | enter to submit answer | ←/→ to navigate questions | esc to interrupt
`
	calls := 0
	classification, err := classifyBlockedTransition(
		context.Background(),
		"codex",
		func(context.Context) (string, error) {
			calls++
			if calls == 1 {
				return partial, nil
			}
			return complete, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 ||
		classification.Kind != question.AttentionQuestion ||
		classification.Interaction == nil ||
		classification.Interaction.Question !=
			"After a respondent finishes a questionnaire, what should the app do?" {
		t.Fatalf("classification after %d reads = %+v", calls, classification)
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

func TestRecentSafeErrorsAreBoundedAndSingleLine(t *testing.T) {
	s := testServer()
	for index := 0; index < 25; index++ {
		s.recordSafeError("component failed", errors.New("safe\nmessage"))
	}
	recent := s.recentSafeErrors()
	if len(recent) != 20 {
		t.Fatalf("recent errors = %d, want 20", len(recent))
	}
	for _, message := range recent {
		if strings.Contains(message, "\n") || !strings.Contains(message, "component failed: safe message") {
			t.Fatalf("unsafe recent error = %q", message)
		}
	}
}

func transitionPushQueue(t *testing.T, s *Server) func() []push.Payload {
	t.Helper()
	directory := t.TempDir()
	manager, err := push.NewManager(directory, s.logger)
	if err != nil {
		t.Fatal(err)
	}
	subscription := push.Subscription{
		Endpoint: "https://fcm.googleapis.com/phone", DeviceID: "device-a",
	}
	subscription.Keys.P256dh = "key"
	subscription.Keys.Auth = "auth"
	if err := manager.Subscribe(subscription); err != nil {
		t.Fatal(err)
	}
	policy := manager.Policy(subscription.DeviceID, "en")
	policy.Categories[push.CategoryFinished] = true
	if err := manager.SetPolicy(policy); err != nil {
		t.Fatal(err)
	}
	s.pushM = manager
	return func() []push.Payload { return queuedPushPayloads(t, directory) }
}

func queuedPushPayloads(t *testing.T, directory string) []push.Payload {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(directory, "queue.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var queue struct {
		Entries map[string]struct {
			Event push.PushEvent `json:"event"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(data, &queue); err != nil {
		t.Fatal(err)
	}
	payloads := make([]push.Payload, 0, len(queue.Entries))
	for _, entry := range queue.Entries {
		var payload push.Payload
		if err := json.Unmarshal(entry.Event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		payloads = append(payloads, payload)
	}
	return payloads
}

type recordingTransitionBroadcast struct {
	messages chan any
}

func (b *recordingTransitionBroadcast) Broadcast(message any) {
	b.messages <- message
}

func TestUnchangedPollPreservesBlockedTransitionSideEffects(t *testing.T) {
	root := t.TempDir()
	s := testServer()
	journal, err := activity.OpenJournal(filepath.Join(root, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	s.dispatcher = coordinator.NewDispatcher(nil, s.state, journal, s.logger)
	t.Cleanup(func() {
		_ = s.dispatcher.Close(context.Background())
	})
	queuedPushes := transitionPushQueue(t, s)
	broadcast := &recordingTransitionBroadcast{messages: make(chan any, 2)}
	s.transitionBroadcast = broadcast
	enrichStarted := make(chan struct{})
	enrichRelease := make(chan struct{})
	s.transitionEnrich = func(_ context.Context, agent *coordinator.AgentState) {
		close(enrichStarted)
		<-enrichRelease
		agent.AttentionKind = "approval"
		agent.Command = "Approve deployment"
		agent.Prompt = "Allow this command?"
		agent.Options = []string{"Approve", "Reject"}
	}
	s.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", TerminalID: "terminal-1", Agent: "codex", Project: "relay", Status: "working",
	}}, s.state.RevisionCounter())
	s.state.CommitEvent("pane-1", "blocked", time.Now().UnixMilli())
	revision := s.state.Revision("pane-1")

	done := make(chan struct{})
	go func() {
		s.handleTransition(context.Background(), "pane-1", "codex", "relay", "blocked", revision, time.Now().UnixMilli())
		close(done)
	}()
	<-enrichStarted
	s.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", TerminalID: "terminal-1", Agent: "codex", Project: "relay", Status: "blocked",
	}}, s.state.RevisionCounter())
	close(enrichRelease)
	<-done

	if entries := journal.Recent(10); len(entries) != 1 ||
		entries[0].Kind != "blocked" || entries[0].Summary != "Approve deployment" {
		t.Fatalf("blocked activity entries = %+v, want exactly one enriched entry", entries)
	}
	payloads := queuedPushes()
	if len(payloads) != 1 {
		t.Fatalf("blocked pushes = %+v, want exactly one", payloads)
	}
	agent, _ := s.state.Agent("pane-1")
	wantKey := push.PushEventKey{
		DeviceID: "device-a", ServerSessionID: "primary", PaneID: agent.PaneID,
		TerminalID: agent.TerminalID, AgentSessionID: agent.SessionID, Generation: agent.Generation,
		EventID: agent.BlockedEventID, InteractionRevision: s.state.AttentionRevision(agent.PaneID),
		Category: push.CategoryAttention,
	}
	if payloads[0].Version != 1 || payloads[0].Key != wantKey || payloads[0].EventRef == "" {
		t.Fatalf("blocked push = %+v, want key %+v with event reference", payloads[0], wantKey)
	}
	if len(broadcast.messages) != 1 {
		t.Fatalf("blocked broadcasts = %d, want exactly one", len(broadcast.messages))
	}
}

func TestBlockedTransitionDoesNotPublishOrBroadcastAfterNewerWorkingState(t *testing.T) {
	s := testServer()
	queuedPushes := transitionPushQueue(t, s)
	broadcast := &recordingTransitionBroadcast{messages: make(chan any, 1)}
	s.transitionBroadcast = broadcast
	enrichStarted := make(chan struct{})
	enrichRelease := make(chan struct{})
	s.transitionEnrich = func(_ context.Context, agent *coordinator.AgentState) {
		close(enrichStarted)
		<-enrichRelease
		agent.AttentionKind = question.AttentionApproval
	}
	s.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", TerminalID: "terminal-1", Agent: "codex", Project: "relay", Status: "working",
	}}, s.state.RevisionCounter())
	s.state.CommitEvent("pane-1", "blocked", time.Now().UnixMilli())
	revision := s.state.Revision("pane-1")

	done := make(chan struct{})
	go func() {
		s.handleTransition(context.Background(), "pane-1", "codex", "relay", "blocked", revision, time.Now().UnixMilli())
		close(done)
	}()
	<-enrichStarted

	s.state.CommitEvent("pane-1", "working", time.Now().UnixMilli())
	close(enrichRelease)
	<-done

	if payloads := queuedPushes(); len(payloads) != 0 {
		t.Fatalf("stale blocked transition queued pushes: %+v", payloads)
	}
	select {
	case message := <-broadcast.messages:
		t.Fatalf("stale blocked transition was broadcast after working revision: %#v", message)
	default:
	}
}

func TestApprovalPushSuppressedWhenAttentionIsReclassified(t *testing.T) {
	s := testServer()
	queuedPushes := transitionPushQueue(t, s)
	broadcast := &recordingTransitionBroadcast{messages: make(chan any, 1)}
	s.transitionBroadcast = broadcast
	enrichStarted := make(chan struct{})
	enrichRelease := make(chan struct{})
	s.transitionEnrich = func(_ context.Context, agent *coordinator.AgentState) {
		close(enrichStarted)
		<-enrichRelease
		agent.AttentionKind = question.AttentionApproval
		agent.Command = "Approve deployment"
		agent.Prompt = "Allow this command?"
		agent.Options = []string{"Approve", "Reject"}
	}
	s.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", TerminalID: "terminal-1", Agent: "codex", Project: "relay", Status: "working",
	}}, s.state.RevisionCounter())
	s.state.CommitEvent("pane-1", "blocked", time.Now().UnixMilli())
	revision := s.state.Revision("pane-1")

	done := make(chan struct{})
	go func() {
		s.handleTransition(context.Background(), "pane-1", "codex", "relay", "blocked", revision, time.Now().UnixMilli())
		close(done)
	}()
	<-enrichStarted

	agent, ok := s.state.Agent("pane-1")
	if !ok {
		t.Fatal("blocked agent disappeared")
	}
	generation, active := s.state.PaneSession("pane-1")
	if !active {
		t.Fatal("blocked pane session disappeared")
	}
	reclassified, committed := s.state.CommitAttentionClassification(
		"pane-1",
		agent.BlockedEventID,
		generation,
		s.state.ContentRevision("pane-1"),
		question.Classification{
			Kind:   question.AttentionChat,
			Prompt: "What would you like to work on next?",
		},
	)
	if !committed {
		t.Fatal("chat reclassification was not committed")
	}
	close(enrichRelease)
	<-done

	if payloads := queuedPushes(); len(payloads) != 0 {
		t.Fatalf("reclassified approval queued pushes: %+v", payloads)
	}
	select {
	case message := <-broadcast.messages:
		t.Fatalf("stale approval was broadcast after reclassification: %#v", message)
	default:
	}

	s.handleChatCompletion(context.Background(), reclassified)
	if payloads := queuedPushes(); len(payloads) != 1 || payloads[0].Key.Category != push.CategoryFinished {
		t.Fatalf("chat reclassification pushes = %+v, want one completion", payloads)
	}
}

func TestChatClassificationUsesOneCompletionPath(t *testing.T) {
	root := t.TempDir()
	s := testServer()
	journal, err := activity.OpenJournal(filepath.Join(root, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	s.dispatcher = coordinator.NewDispatcher(nil, s.state, journal, s.logger)
	t.Cleanup(func() {
		_ = s.dispatcher.Close(context.Background())
	})
	queuedPushes := transitionPushQueue(t, s)
	broadcast := &recordingTransitionBroadcast{messages: make(chan any, 3)}
	s.transitionBroadcast = broadcast
	s.transitionEnrich = func(_ context.Context, agent *coordinator.AgentState) {
		agent.AttentionKind = "chat"
		agent.Prompt = "Hello! What would you like to work on next?"
		agent.Options = []string{"fabricated", "controls"}
	}
	s.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", TerminalID: "terminal-1", Agent: "codex", Project: "relay", Status: "working",
	}}, s.state.RevisionCounter())
	s.state.CommitEvent("pane-1", "blocked", time.Now().UnixMilli())
	s.handleTransition(
		context.Background(), "pane-1", "codex", "relay", "blocked",
		s.state.Revision("pane-1"), time.Now().UnixMilli(),
	)

	entries := journal.Recent(10)
	if len(entries) != 1 || entries[0].Kind != "finished" ||
		entries[0].Extract != "Hello! What would you like to work on next?" {
		t.Fatalf("chat activities = %+v, want one completion", entries)
	}
	payloads := queuedPushes()
	if len(payloads) != 1 {
		t.Fatalf("chat pushes = %+v, want one completion push", payloads)
	}
	payload := payloads[0]
	if payload.Key.Category != push.CategoryFinished || !strings.Contains(payload.Title, "finished") ||
		len(payload.Actions) != 0 || len(payload.ActionRefs) != 0 {
		t.Fatalf("chat push = %+v", payload)
	}
	message := (<-broadcast.messages).(map[string]any)
	if fmt.Sprint(message["attention_kind"]) != "chat" {
		t.Fatalf("chat broadcast = %+v", message)
	}
	if options, ok := message["options"].([]string); ok && len(options) != 0 {
		t.Fatalf("chat broadcast retained controls: %+v", message)
	}

	s.state.CommitEvent("pane-1", "idle", time.Now().UnixMilli())
	s.handleTransition(
		context.Background(), "pane-1", "codex", "relay", "idle",
		s.state.Revision("pane-1"), time.Now().UnixMilli(),
	)
	if len(journal.Recent(10)) != 1 || len(queuedPushes()) != 1 {
		t.Fatal("raw idle duplicated the classified chat completion")
	}
}

func TestUnknownClassificationHasNoNotificationActions(t *testing.T) {
	root := t.TempDir()
	s := testServer()
	journal, err := activity.OpenJournal(filepath.Join(root, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	s.dispatcher = coordinator.NewDispatcher(nil, s.state, journal, s.logger)
	t.Cleanup(func() {
		_ = s.dispatcher.Close(context.Background())
	})
	queuedPushes := transitionPushQueue(t, s)
	s.transitionBroadcast = &recordingTransitionBroadcast{messages: make(chan any, 1)}
	s.transitionEnrich = func(_ context.Context, agent *coordinator.AgentState) {
		agent.AttentionKind = "unknown"
		agent.Prompt = "Agent needs inspection"
		agent.Options = []string{"Approve", "Reject"}
	}
	s.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", TerminalID: "terminal-1", Agent: "opencode", Project: "relay", Status: "working",
	}}, s.state.RevisionCounter())
	s.state.CommitEvent("pane-1", "blocked", time.Now().UnixMilli())
	s.handleTransition(
		context.Background(), "pane-1", "opencode", "relay", "blocked",
		s.state.Revision("pane-1"), time.Now().UnixMilli(),
	)

	payloads := queuedPushes()
	if len(payloads) != 1 {
		t.Fatalf("unknown pushes = %+v, want one inspection push", payloads)
	}
	payload := payloads[0]
	if payload.Key.Category != push.CategoryAttention || payload.Title != "Herdr notification" ||
		len(payload.Actions) != 0 || len(payload.ActionRefs) != 0 {
		t.Fatalf("unknown push = %+v", payload)
	}
	agent, _ := s.state.Agent("pane-1")
	if len(agent.Options) != 0 || agent.Interaction != nil {
		t.Fatalf("unknown classification retained controls: %+v", agent)
	}
}
