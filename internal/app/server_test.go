package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/activity"
	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/conversation"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/copyresponse"
	"github.com/0cv/herdr-mobile-relay/internal/panedelta"
	"github.com/0cv/herdr-mobile-relay/internal/question"
	"github.com/0cv/herdr-mobile-relay/internal/session"
	"github.com/0cv/herdr-mobile-relay/internal/slashcmd"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
	"github.com/coder/websocket"
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
	s.sessions = session.NewResolver(home)

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
	s.sessions = session.NewResolver(home)

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

func TestCaptureFinishedPanePrefersConversationResponse(t *testing.T) {
	home := t.TempDir()
	sessionID := "01a00af4-9706-7000-81b5-390a66466563"
	path := filepath.Join(home, ".omp", "agent", "sessions", "-relay", "session_"+sessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(file)
	rows := []map[string]any{
		{
			"type": "message",
			"message": map[string]any{
				"role":    "user",
				"content": []any{map[string]any{"type": "text", "text": "Review the change"}},
			},
		},
		{
			"type":      "message",
			"timestamp": "2026-08-16T14:24:45.112Z",
			"message": map[string]any{
				"role": "assistant",
				"content": []any{map[string]any{"type": "text", "text": strings.Join([]string{
					"Here is the complete response.",
					"",
					"1. The first detail.",
					"2. The second detail.",
					"3. The third detail.",
					"4. The fourth detail.",
					"5. The fifth detail.",
					"6. The sixth detail.",
					"7. The seventh detail.",
					"8. The eighth detail.",
					"9. The ninth detail.",
					"10. The tenth detail.",
					"11. The eleventh detail.",
					"12. The twelfth detail.",
				}, "\n")}},
			},
		},
	}
	for _, row := range rows {
		if err := encoder.Encode(row); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	s := testServer()
	s.conversationM = conversation.NewReader(home)
	want := rows[1]["message"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if got := s.captureFinishedPane(context.Background(), "pane-1", "omp", sessionID); got != want {
		t.Fatalf("captured response = %q, want full conversation response %q", got, want)
	}

	s.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", Agent: "omp", Status: "idle", SessionID: path,
	}}, s.state.RevisionCounter())
	s.activityView = []activity.Entry{{
		ID: "old-finished", Timestamp: activity.MilliTimestamp(time.Date(2026, time.August, 16, 14, 24, 46, 0, time.UTC).UnixMilli()),
		Kind: "finished", Status: "completed", Agent: "omp", PaneID: "pane-1", Session: "Old title",
	}}
	backfilled := s.recentActivities(1)
	if len(backfilled) != 1 || backfilled[0].Extract != want {
		t.Fatalf("backfilled activity = %#v, want full conversation response", backfilled)
	}
}

func TestHealth(t *testing.T) {
	s := testServer()
	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()

	s.handleHealth(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if body := w.Body.String(); body != "ok\n" {
		t.Errorf("body = %q, want \"ok\\n\"", body)
	}
	if inst := w.Header().Get("X-Herdr-Relay-Instance"); inst != "test-instance" {
		t.Errorf("instance header = %q, want test-instance", inst)
	}
}

func TestPaneDeltaResponsePreservesTruncation(t *testing.T) {
	delta := paneDeltaResponse(
		map[string]any{
			"type":      "pane_content",
			"content":   "new output",
			"truncated": true,
			"format":    "ansi",
		},
		"content-1",
		nil,
	)
	if delta["truncated"] != true {
		t.Fatalf("delta truncation = %#v, want true", delta["truncated"])
	}
	if _, ok := delta["content"]; ok {
		t.Fatal("delta unexpectedly included full content")
	}
}

func TestPaneWatchUpdateSendsMetadataOnlyDelta(t *testing.T) {
	response := map[string]any{
		"type":           "pane_content",
		"content":        "unchanged\nquestion\n",
		"attention_kind": question.AttentionQuestion,
		"interaction":    map[string]any{"id": "question-1"},
	}
	previous := map[string]any{
		"type":           "pane_content",
		"content":        "unchanged\nquestion\n",
		"attention_kind": question.AttentionUnknown,
	}
	acknowledged := &paneWatchFrame{
		content:            "unchanged\nquestion\n",
		contentFingerprint: "content-1",
		frameFingerprint:   paneFrameFingerprint(previous),
	}
	current := &paneWatchFrame{
		content:            "unchanged\nquestion\n",
		contentFingerprint: "content-1",
		frameFingerprint:   paneFrameFingerprint(response),
	}
	if acknowledged.frameFingerprint == current.frameFingerprint {
		t.Fatal("question metadata did not change the pane frame fingerprint")
	}
	initial := *acknowledged
	initial.frameFingerprint = ""
	if paneWatchUpdate(response, &initial, current) == nil {
		t.Fatal("initial pane watch suppressed current interaction metadata")
	}

	update := paneWatchUpdate(response, acknowledged, current)
	if update["type"] != "pane_delta" ||
		update["base_fingerprint"] != "content-1" {
		t.Fatalf("metadata update = %#v", update)
	}
	if _, ok := update["content"]; ok {
		t.Fatal("metadata delta unexpectedly included full terminal content")
	}
	segments, ok := update["segments"].([]panedelta.Segment)
	if !ok || len(segments) != 1 || segments[0].CopyStart != 0 ||
		segments[0].CopyLines != 3 || segments[0].Text != "" {
		t.Fatalf("metadata delta segments = %#v, want one whole-frame copy", update["segments"])
	}
	applied, appliedOK := panedelta.Apply(acknowledged.content, segments)
	if !appliedOK || applied != current.content {
		t.Fatalf("metadata delta applied = %q, %v; want %q, true", applied, appliedOK, current.content)
	}
	encoded, err := json.Marshal(update)
	if err != nil {
		t.Fatalf("marshal metadata delta: %v", err)
	}
	if strings.Contains(string(encoded), `"segments":null`) {
		t.Fatalf("metadata delta encoded a null segment list: %s", encoded)
	}
}

func TestPaneWatchUpdateSendsResizeSettledDelta(t *testing.T) {
	settlingResponse := map[string]any{
		"type":            "pane_content",
		"content":         "unchanged terminal",
		"resize_settling": true,
	}
	settledResponse := map[string]any{
		"type":    "pane_content",
		"content": "unchanged terminal",
	}
	acknowledged := &paneWatchFrame{
		content:            "unchanged terminal",
		contentFingerprint: "content-1",
		frameFingerprint:   paneFrameFingerprint(settlingResponse),
		resizeSettling:     true,
	}
	current := &paneWatchFrame{
		content:            "unchanged terminal",
		contentFingerprint: "content-1",
		frameFingerprint:   paneFrameFingerprint(settledResponse),
	}
	if acknowledged.frameFingerprint == current.frameFingerprint {
		t.Fatal("resize settling state did not change the pane frame fingerprint")
	}

	if !paneWatchNeedsFrameRead("probe-1", "probe-1", acknowledged, "") {
		t.Fatal("settling pane frame would not be re-read after an unchanged probe")
	}
	if paneWatchNeedsFrameRead("probe-1", "probe-1", current, "") {
		t.Fatal("settled pane frame would be re-read after an unchanged probe")
	}
	update := paneWatchUpdate(settledResponse, acknowledged, current)
	if update["type"] != "pane_delta" ||
		update["base_fingerprint"] != "content-1" {
		t.Fatalf("resize settled update = %#v", update)
	}
	if update["resize_settling"] == true {
		t.Fatalf("resize settled update remained transient: %#v", update)
	}
}

func TestPreparePaneResponsePreservesHistoryDuringResizeSession(t *testing.T) {
	s := testServerWithCacheDir(t.TempDir())
	s.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", Agent: "claude", Status: "idle",
	}}, s.state.RevisionCounter())
	baseline := "history 1\nhistory 2\nhistory 3\nhistory 4\nhistory 5\nhistory 6\nhistory 7\nhistory 8"
	s.historyM.Merge("pane-1", baseline)

	response := map[string]any{
		"type": "pane_content", "pane_id": "pane-1",
		"content": "current 1\ncurrent 2", "format": "ansi",
		"truncated": false, "viewport_only": true,
	}
	s.preparePaneResponse(
		map[string]any{"pane_id": "pane-1", "lines": 100, "terminal_columns": 59},
		response,
	)

	if response["content"] != "current 1\ncurrent 2" {
		t.Fatalf("resized content = %q, want clean current viewport", response["content"])
	}
	if response["truncated"] != false {
		t.Fatalf("resized truncation = %#v, want false", response["truncated"])
	}
	if content := s.historyM.Content("pane-1", 100); content != baseline {
		t.Fatalf("history changed during resized read:\n%s", content)
	}
}

func TestCopyBlockedMessage(t *testing.T) {
	tests := []struct {
		name  string
		agent *coordinator.AgentState
		want  string
	}{
		{name: "nil", want: ""},
		{name: "working question", agent: &coordinator.AgentState{
			Status: "working", AttentionKind: question.AttentionQuestion,
		}, want: ""},
		{name: "question", agent: &coordinator.AgentState{
			Status: "blocked", AttentionKind: question.AttentionQuestion,
		}, want: "Agent is waiting for an answer"},
		{name: "approval", agent: &coordinator.AgentState{
			Status: "blocked", AttentionKind: question.AttentionApproval,
		}, want: "Agent is waiting for approval"},
		{name: "unknown blocked", agent: &coordinator.AgentState{
			Status: "blocked", AttentionKind: question.AttentionUnknown,
		}, want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := copyBlockedMessage(test.agent); got != test.want {
				t.Fatalf("copyBlockedMessage() = %q, want %q", got, test.want)
			}
		})
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

func TestCopyResponseError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "busy composer", err: copyresponse.ErrComposerBusy, want: "The agent composer is busy; finish or clear the current prompt first"},
		{name: "open picker", err: copyresponse.ErrPickerOpen, want: "The agent already has a copy menu open; close it and try again"},
		{name: "stale output", err: copyresponse.ErrStaleOutput, want: "The copied response changed before it could be read; try again"},
		{name: "no copy", err: copyresponse.ErrNoCopy, want: "The agent did not confirm a copied response; try again"},
		{name: "timeout", err: context.DeadlineExceeded, want: "Copying the agent response timed out; try again"},
		{name: "unknown", err: errors.New("internal detail"), want: "Could not copy the agent response; try again"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := copyResponseError(test.err); got != test.want {
				t.Fatalf("copyResponseError() = %q, want %q", got, test.want)
			}
		})
	}
}
func openCopyTestClient(t *testing.T, s *Server) *websocket.Conn {
	t.Helper()
	s.hub.SetHandler(func(client *transport.ClientConn, message map[string]any, admitted func()) {
		defer admitted()
		if message["type"] != "copy_agent_response" {
			return
		}
		requestID, _ := message["request_id"].(string)
		paneID, _ := message["pane_id"].(string)
		s.copyAgentResponse(client, requestID, paneID)
	})
	server := httptest.NewServer(http.HandlerFunc(s.hub.HandleWebSocket))
	conn, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		server.Close()
		t.Fatalf("dial copy test client: %v", err)
	}
	t.Cleanup(func() {
		conn.CloseNow()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.hub.Shutdown(shutdownCtx)
		server.Close()
	})
	return conn
}

func sendCopyRequest(t *testing.T, conn *websocket.Conn, requestID, paneID string) map[string]any {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"type": "copy_agent_response", "request_id": requestID, "pane_id": paneID,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
		t.Fatalf("write copy request: %v", err)
	}
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read copy result: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("decode copy result: %v", err)
	}
	return result
}

func TestCopyAgentResponseValidatesPaneState(t *testing.T) {
	tests := []struct {
		name    string
		paneID  string
		setup   func(*Server)
		wantErr string
	}{
		{name: "missing pane id", wantErr: "Agent is required"},
		{name: "missing pane", paneID: "missing", wantErr: "Agent pane not found"},
		{
			name:   "clipboard unavailable",
			paneID: "pane-1",
			setup: func(s *Server) {
				s.state.CommitInventory([]*coordinator.AgentState{{PaneID: "pane-1", Agent: "claude", Status: "idle"}}, s.state.RevisionCounter())
				s.clipboardRead = nil
				s.clipboardWrite = nil
			},
			wantErr: "Host clipboard is unavailable",
		},
		{
			name:   "unsupported agent",
			paneID: "pane-1",
			setup: func(s *Server) {
				s.state.CommitInventory([]*coordinator.AgentState{{PaneID: "pane-1", Agent: "unknown", Status: "idle"}}, s.state.RevisionCounter())
				s.profiles.Remember("pane-1", "unknown")
				s.clipboardRead = func(context.Context) ([]byte, error) { return nil, nil }
				s.clipboardWrite = func(context.Context, []byte) error { return nil }
			},
			wantErr: "Agent does not support response copying",
		},
		{
			name:   "working agent",
			paneID: "pane-1",
			setup: func(s *Server) {
				s.state.CommitInventory([]*coordinator.AgentState{{PaneID: "pane-1", Agent: "claude", Status: "working"}}, s.state.RevisionCounter())
				s.clipboardRead = func(context.Context) ([]byte, error) { return nil, nil }
				s.clipboardWrite = func(context.Context, []byte) error { return nil }
			},
			wantErr: "Agent is still working; wait for the current turn to finish",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := testServer()
			if test.setup != nil {
				test.setup(s)
			}
			result := sendCopyRequest(t, openCopyTestClient(t, s), test.name, test.paneID)
			if got, _ := result["error"].(string); got != test.wantErr {
				t.Fatalf("copy error = %q, want %q; response = %#v", got, test.wantErr, result)
			}
		})
	}
}

func TestCopyAgentResponseRejectsReplacedPane(t *testing.T) {
	s := testServer()
	const paneID = "pane-1"
	s.state.CommitInventory([]*coordinator.AgentState{
		{PaneID: paneID, Agent: "claude", Status: "idle", PaneRevision: 4},
	}, s.state.RevisionCounter())
	s.profiles.Remember(paneID, "claude")
	s.clipboardRead = func(context.Context) ([]byte, error) { return []byte("before"), nil }
	s.clipboardWrite = func(context.Context, []byte) error { return nil }
	s.copyRunner = func(
		_ context.Context,
		paneID string,
		_ slashcmd.CopyProfile,
		_ copyresponse.Pane,
		_ copyresponse.ClipboardReader,
		_ copyresponse.ClipboardWriter,
		_ int64,
		_ copyresponse.RevisionReader,
	) (copyresponse.Result, error) {
		s.state.BumpGeneration(paneID)
		return copyresponse.Result{Text: "response", Source: "clipboard", Chars: 8, Lines: 1}, nil
	}
	result := sendCopyRequest(t, openCopyTestClient(t, s), "replaced", paneID)
	if got, _ := result["error"].(string); got != "The agent pane was replaced while the response was being copied" {
		t.Fatalf("copy error = %q; response = %#v", got, result)
	}
}

func TestCopyAgentResponseReturnsCopiedData(t *testing.T) {
	s := testServer()
	const paneID = "pane-1"
	s.state.CommitInventory([]*coordinator.AgentState{
		{PaneID: paneID, Agent: "claude", Status: "idle", PaneRevision: 4},
	}, s.state.RevisionCounter())
	s.profiles.Remember(paneID, "claude")
	s.clipboardRead = func(context.Context) ([]byte, error) { return []byte("before"), nil }
	s.clipboardWrite = func(context.Context, []byte) error { return nil }
	s.copyRunner = func(
		context.Context,
		string,
		slashcmd.CopyProfile,
		copyresponse.Pane,
		copyresponse.ClipboardReader,
		copyresponse.ClipboardWriter,
		int64,
		copyresponse.RevisionReader,
	) (copyresponse.Result, error) {
		return copyresponse.Result{Text: "response", Source: "clipboard", Chars: 8, Lines: 1}, nil
	}
	result := sendCopyRequest(t, openCopyTestClient(t, s), "success", paneID)
	if ok, _ := result["ok"].(bool); !ok {
		t.Fatalf("copy result = %#v, want success", result)
	}
	data, _ := result["data"].(map[string]any)
	if data["text"] != "response" || data["source"] != "clipboard" {
		t.Fatalf("copy data = %#v", data)
	}
}

func TestHealthz(t *testing.T) {
	s := testServer()
	s.ready = true
	s.state.CommitInventory(nil, 0)
	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()

	s.handleHealthz(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["status"] != "ok" {
		t.Errorf("status = %v, want ok", resp["status"])
	}
	if resp["readiness"] != "ready" {
		t.Errorf("readiness = %v, want ready", resp["readiness"])
	}
	if resp["instance"] != "test-instance" {
		t.Errorf("instance = %v, want test-instance", resp["instance"])
	}
	if resp["release_version"] != "0.9.0" {
		t.Errorf("release_version = %v, want 0.9.0", resp["release_version"])
	}
	if resp["revision"] != "abc123" {
		t.Errorf("revision = %v, want abc123", resp["revision"])
	}
	if resp["protocol"] != float64(2) {
		t.Errorf("protocol = %v, want 2", resp["protocol"])
	}
	if resp["gateway_available_version"] != "0.9.0" {
		t.Errorf("gateway_available_version = %v, want 0.9.0", resp["gateway_available_version"])
	}
}

func TestReadyzNotReady(t *testing.T) {
	s := testServer()
	s.ready = false
	req := httptest.NewRequest("GET", "/readyz", nil)
	w := httptest.NewRecorder()

	s.handleReadyz(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "unavailable" {
		t.Errorf("status = %v, want unavailable", resp["status"])
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

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned error: %v", err)
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

func TestRequestedPaneWatchInterval(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  time.Duration
	}{
		{name: "fast", value: float64(100), want: 100 * time.Millisecond},
		{name: "default", value: float64(250), want: 250 * time.Millisecond},
		{name: "balanced battery", value: float64(500), want: 500 * time.Millisecond},
		{name: "slow", value: float64(1_000), want: time.Second},
		{name: "missing", value: nil, want: defaultPaneWatchInterval},
		{name: "unsupported", value: float64(333), want: defaultPaneWatchInterval},
		{name: "fractional", value: 100.5, want: defaultPaneWatchInterval},
		{name: "wrong type", value: "100", want: defaultPaneWatchInterval},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := requestedPaneWatchInterval(test.value); got != test.want {
				t.Fatalf("requestedPaneWatchInterval(%v) = %s, want %s", test.value, got, test.want)
			}
		})
	}
}

func TestCommittedActivityViewTracksLiveCommitAndClear(t *testing.T) {
	s := testServer()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.hub.Shutdown(ctx)
	})
	entry := activity.NewEntry("prompt", "sent", "hello", "pane-1", "", "", "request-1")
	s.broadcastCommitted(map[string]any{"type": "activity", "activity": entry})
	recent := s.recentActivities(500)
	if len(recent) != 1 || recent[0].ID != entry.ID {
		t.Fatalf("committed view = %+v", recent)
	}
	s.broadcastCommitted(map[string]any{"type": "activity_history", "activities": []activity.Entry{}})
	if recent := s.recentActivities(500); len(recent) != 0 {
		t.Fatalf("cleared view = %+v", recent)
	}
}

func TestCommittedStateViewTracksSnapshotsAndDeltas(t *testing.T) {
	s := testServer()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.hub.Shutdown(ctx)
	})
	s.broadcastCommitted(map[string]any{
		"type": "agents",
		"agents": []*coordinator.AgentState{{
			PaneID: "pane-1", Status: "working", Project: "project",
		}},
	})
	s.broadcastCommitted(map[string]any{
		"type": "agent_update", "pane_id": "pane-1", "status": "blocked", "event_id": "event-1",
	})
	agents := s.committedAgents()
	if len(agents) != 1 || agents[0].Status != "blocked" || agents[0].BlockedEventID != "event-1" {
		t.Fatalf("committed agents = %+v", agents)
	}
	s.broadcastCommitted(map[string]any{
		"type": "inventory_status", "state": "ready", "stale": false,
	})
	inventory := s.committedInventoryStatus()
	if inventory["state"] != "ready" || inventory["stale"] != false {
		t.Fatalf("committed inventory = %+v", inventory)
	}
	if _, leaked := inventory["type"]; leaked {
		t.Fatalf("inventory view leaked transport type: %+v", inventory)
	}
}

func TestCommittedStateViewRejectsStalePerPaneUpdates(t *testing.T) {
	s := testServer()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.hub.Shutdown(ctx)
	})
	s.broadcastCommitted(map[string]any{
		"type": "agents",
		"agents": []*coordinator.AgentState{{
			PaneID: "pane-1", Status: "working", StateRevision: 12,
		}},
	})
	s.broadcastCommitted(map[string]any{
		"type": "agent_update", "pane_id": "pane-1", "status": "blocked",
		"event_id": "stale-event", "pane_revision": int64(11),
	})
	s.broadcastCommitted(map[string]any{
		"type": "agents",
		"agents": []*coordinator.AgentState{{
			PaneID: "pane-1", Status: "blocked", BlockedEventID: "stale-snapshot", StateRevision: 10,
		}},
	})

	agents := s.committedAgents()
	if len(agents) != 1 || agents[0].Status != "working" ||
		agents[0].StateRevision != 12 || agents[0].BlockedEventID != "" {
		t.Fatalf("reconnect snapshot regressed after stale messages: %+v", agents)
	}
}

type blockingTransitionPush struct {
	started chan struct{}
	release chan struct{}
	cancel  chan struct{}
}

func (p *blockingTransitionPush) Send(ctx context.Context, _ []byte) {
	close(p.started)
	select {
	case <-p.release:
	case <-ctx.Done():
		if p.cancel != nil {
			close(p.cancel)
		}
	}
}

type recordingTransitionBroadcast struct {
	messages chan any
}

func (b *recordingTransitionBroadcast) Broadcast(message any) {
	b.messages <- message
}

type recordingTransitionPush struct {
	messages chan []byte
}

func (p *recordingTransitionPush) Send(_ context.Context, payload []byte) {
	p.messages <- payload
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
	push := &recordingTransitionPush{messages: make(chan []byte, 2)}
	broadcast := &recordingTransitionBroadcast{messages: make(chan any, 2)}
	s.transitionPush = push
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
		PaneID: "pane-1", Agent: "codex", Project: "relay", Status: "working",
	}}, s.state.RevisionCounter())
	s.state.CommitEvent("pane-1", "blocked", time.Now().UnixMilli())
	revision := s.state.Revision("pane-1")

	done := make(chan struct{})
	go func() {
		s.handleTransition(context.Background(), "pane-1", "codex", "relay", "blocked", revision)
		close(done)
	}()
	<-enrichStarted
	s.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", Agent: "codex", Project: "relay", Status: "blocked",
	}}, s.state.RevisionCounter())
	close(enrichRelease)
	<-done

	if entries := journal.Recent(10); len(entries) != 1 ||
		entries[0].Kind != "blocked" || entries[0].Summary != "Approve deployment" {
		t.Fatalf("blocked activity entries = %+v, want exactly one enriched entry", entries)
	}
	if len(push.messages) != 1 {
		t.Fatalf("blocked pushes = %d, want exactly one", len(push.messages))
	}
	if len(broadcast.messages) != 1 {
		t.Fatalf("blocked broadcasts = %d, want exactly one", len(broadcast.messages))
	}
}

func TestBlockedBroadcastDoesNotOvertakeNewerWorkingState(t *testing.T) {
	s := testServer()
	push := &blockingTransitionPush{
		started: make(chan struct{}), release: make(chan struct{}), cancel: make(chan struct{}),
	}
	broadcast := &recordingTransitionBroadcast{messages: make(chan any, 1)}
	s.transitionPush = push
	s.transitionBroadcast = broadcast
	s.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", Agent: "codex", Project: "relay", Status: "working",
	}}, s.state.RevisionCounter())
	s.state.CommitEvent("pane-1", "blocked", time.Now().UnixMilli())
	revision := s.state.Revision("pane-1")

	done := make(chan struct{})
	go func() {
		s.handleTransition(context.Background(), "pane-1", "codex", "relay", "blocked", revision)
		close(done)
	}()
	<-push.started

	s.state.CommitEvent("pane-1", "working", time.Now().UnixMilli())
	select {
	case <-push.cancel:
	case <-time.After(time.Second):
		t.Fatal("stale blocked push request was not canceled")
	}
	<-done

	select {
	case message := <-broadcast.messages:
		t.Fatalf("stale blocked transition was broadcast after working revision: %#v", message)
	default:
	}
}

func TestApprovalPushCanceledWhenAttentionIsReclassified(t *testing.T) {
	s := testServer()
	push := &blockingTransitionPush{
		started: make(chan struct{}), release: make(chan struct{}), cancel: make(chan struct{}),
	}
	broadcast := &recordingTransitionBroadcast{messages: make(chan any, 1)}
	s.transitionPush = push
	s.transitionBroadcast = broadcast
	s.transitionEnrich = func(_ context.Context, agent *coordinator.AgentState) {
		agent.AttentionKind = question.AttentionApproval
		agent.Command = "Approve deployment"
		agent.Prompt = "Allow this command?"
		agent.Options = []string{"Approve", "Reject"}
	}
	s.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", Agent: "codex", Project: "relay", Status: "working",
	}}, s.state.RevisionCounter())
	s.state.CommitEvent("pane-1", "blocked", time.Now().UnixMilli())
	revision := s.state.Revision("pane-1")

	done := make(chan struct{})
	go func() {
		s.handleTransition(context.Background(), "pane-1", "codex", "relay", "blocked", revision)
		close(done)
	}()
	<-push.started

	agent, ok := s.state.Agent("pane-1")
	if !ok {
		t.Fatal("blocked agent disappeared")
	}
	generation, active := s.state.PaneSession("pane-1")
	if !active {
		t.Fatal("blocked pane session disappeared")
	}
	if _, committed := s.state.CommitAttentionClassification(
		"pane-1",
		agent.BlockedEventID,
		uint64(generation),
		s.state.ContentRevision("pane-1"),
		question.Classification{
			Kind:   question.AttentionChat,
			Prompt: "What would you like to work on next?",
		},
	); !committed {
		t.Fatal("chat reclassification was not committed")
	}
	select {
	case <-push.cancel:
	case <-time.After(time.Second):
		t.Fatal("reclassified approval push request was not canceled")
	}
	<-done

	select {
	case message := <-broadcast.messages:
		t.Fatalf("stale approval was broadcast after reclassification: %#v", message)
	default:
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
	push := &recordingTransitionPush{messages: make(chan []byte, 3)}
	broadcast := &recordingTransitionBroadcast{messages: make(chan any, 3)}
	s.transitionPush = push
	s.transitionBroadcast = broadcast
	s.transitionEnrich = func(_ context.Context, agent *coordinator.AgentState) {
		agent.AttentionKind = "chat"
		agent.Prompt = "Hello! What would you like to work on next?"
		agent.Options = []string{"fabricated", "controls"}
	}
	s.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", Agent: "codex", Project: "relay", Status: "working",
	}}, s.state.RevisionCounter())
	s.state.CommitEvent("pane-1", "blocked", time.Now().UnixMilli())
	s.handleTransition(
		context.Background(), "pane-1", "codex", "relay", "blocked",
		s.state.Revision("pane-1"),
	)

	entries := journal.Recent(10)
	if len(entries) != 1 || entries[0].Kind != "finished" ||
		entries[0].Extract != "Hello! What would you like to work on next?" {
		t.Fatalf("chat activities = %+v, want one completion", entries)
	}
	if len(push.messages) != 1 {
		t.Fatalf("chat pushes = %d, want one completion push", len(push.messages))
	}
	var payload map[string]any
	if err := json.Unmarshal(<-push.messages, &payload); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fmt.Sprint(payload["title"]), "finished") ||
		len(payload["actions"].([]any)) != 0 {
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
		s.state.Revision("pane-1"),
	)
	if len(journal.Recent(10)) != 1 || len(push.messages) != 0 {
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
	push := &recordingTransitionPush{messages: make(chan []byte, 1)}
	s.transitionPush = push
	s.transitionBroadcast = &recordingTransitionBroadcast{messages: make(chan any, 1)}
	s.transitionEnrich = func(_ context.Context, agent *coordinator.AgentState) {
		agent.AttentionKind = "unknown"
		agent.Prompt = "Agent needs inspection"
		agent.Options = []string{"Approve", "Reject"}
	}
	s.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", Agent: "opencode", Project: "relay", Status: "working",
	}}, s.state.RevisionCounter())
	s.state.CommitEvent("pane-1", "blocked", time.Now().UnixMilli())
	s.handleTransition(
		context.Background(), "pane-1", "opencode", "relay", "blocked",
		s.state.Revision("pane-1"),
	)

	var payload map[string]any
	if err := json.Unmarshal(<-push.messages, &payload); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fmt.Sprint(payload["title"]), "needs inspection") ||
		len(payload["actions"].([]any)) != 0 {
		t.Fatalf("unknown push = %+v", payload)
	}
	agent, _ := s.state.Agent("pane-1")
	if len(agent.Options) != 0 || agent.Interaction != nil {
		t.Fatalf("unknown classification retained controls: %+v", agent)
	}
}

func TestBackgroundClaudeHistoryCaptureDoesNotRequirePhoneRead(t *testing.T) {
	root := t.TempDir()
	fakeHerdr := filepath.Join(root, "herdr")
	script := "#!/bin/sh\nprintf 'first output\\nsecond output\\nfooter 1\\nfooter 2\\nfooter 3\\nfooter 4\\nfooter 5\\nfooter 6\\n'\n"
	if err := os.WriteFile(fakeHerdr, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		HerdrBin:   fakeHerdr,
		CacheDir:   filepath.Join(root, "cache"),
		RuntimeDir: filepath.Join(root, "runtime"),
	}
	s := New(cfg, "0.9.0", "abc123", slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.historyTasks = newLifecycleTasks(context.Background())
	defer s.historyTasks.Stop()
	s.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", Agent: "Claude Code", Status: "working",
	}}, s.state.RevisionCounter())
	s.syncHistoryPanes(s.state.Snapshot())

	s.scheduleHistoryCapture(context.Background(), "pane-1")
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(s.historyM.Content("pane-1", 100), "second output") {
		if time.Now().After(deadline) {
			t.Fatal("background capture did not persist Claude pane output")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRemovedPaneHistoryIsDiscarded(t *testing.T) {
	root := t.TempDir()
	cfg := &config.Config{
		CacheDir:   filepath.Join(root, "cache"),
		RuntimeDir: filepath.Join(root, "runtime"),
	}
	s := New(cfg, "0.9.0", "abc123", slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.historyM.Merge("pane-1", "one\ntwo\nthree\nfour\nfive\nsix\nseven")
	s.historyCaptureMu.Lock()
	s.historyActive["pane-1"] = true
	s.historyLast["pane-1"] = time.Now()
	s.historyCaptureMu.Unlock()

	s.syncHistoryPanes(nil)

	s.historyCaptureMu.Lock()
	_, active := s.historyActive["pane-1"]
	_, last := s.historyLast["pane-1"]
	s.historyCaptureMu.Unlock()
	if active || last {
		t.Fatalf("removed pane tracking remains: active=%v last=%v", active, last)
	}
	files, err := filepath.Glob(filepath.Join(cfg.CacheDir, "claude-history", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("removed pane history files remain: %v", files)
	}
}

func TestFirstInventoryReconcilesHistoryFromEarlierProcess(t *testing.T) {
	root := t.TempDir()
	cfg := &config.Config{
		CacheDir:   filepath.Join(root, "cache"),
		RuntimeDir: filepath.Join(root, "runtime"),
	}
	earlier := New(cfg, "0.9.0", "abc123", slog.New(slog.NewTextHandler(io.Discard, nil)))
	earlier.historyM.Merge("removed-pane", "one\ntwo\nthree\nfour\nfive\nsix\nseven")
	earlier.historyM.SaveAll()

	restarted := New(cfg, "0.9.0", "abc123", slog.New(slog.NewTextHandler(io.Discard, nil)))
	restarted.syncHistoryPanes([]*coordinator.AgentState{{
		PaneID: "active-pane",
		Agent:  "Claude Code",
		Status: "working",
	}})

	files, err := filepath.Glob(filepath.Join(cfg.CacheDir, "claude-history", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("stale history from earlier process remains: %v", files)
	}
}

func TestUnchangedPaneResponseSuppressesTerminalContent(t *testing.T) {
	response := map[string]any{
		"type":    "pane_content",
		"pane_id": "w1:p1",
		"content": "unchanged output",
		"format":  "ansi",
	}
	fingerprint := paneFingerprint("unchanged output")
	unchanged := unchangedPaneResponse(
		map[string]any{"content_fingerprint": fingerprint},
		response,
	)
	if unchanged == nil {
		t.Fatal("matching terminal content was not suppressed")
	}
	if unchanged["type"] != "pane_unchanged" || unchanged["pane_id"] != "w1:p1" {
		t.Fatalf("unexpected unchanged response: %#v", unchanged)
	}
	if _, included := unchanged["content"]; included {
		t.Fatalf("unchanged response included terminal content: %#v", unchanged)
	}
	if response["content_fingerprint"] != fingerprint {
		t.Fatalf("full response fingerprint = %v, want %s", response["content_fingerprint"], fingerprint)
	}

	changed := unchangedPaneResponse(
		map[string]any{"content_fingerprint": "older"},
		response,
	)
	if changed != nil {
		t.Fatalf("changed terminal content was suppressed: %#v", changed)
	}
}
