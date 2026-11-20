package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/activity"
	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/conversation"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/coder/websocket"
)

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
	if got := s.captureFinishedPane(context.Background(), "pane-1", "omp", "", sessionID, conversation.ProjectContext{}); got != want {
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

func TestCaptureFinishedPaneUsesOriginalConversationCwd(t *testing.T) {
	home := t.TempDir()
	const sessionID = "123e4567-e89b-12d3-a456-426614174321"
	oldCWD := "/work/old-pane"
	oldForeground := "/work/old-foreground"
	newCWD := "/work/new-pane"
	newForeground := "/work/new-foreground"
	writeClaudeTranscriptAnswering(t,
		filepath.Join(home, ".claude", "projects", "-work-old-foreground", sessionID+".jsonl"),
		"Old work", "answer from original cwd")
	writeClaudeTranscriptAnswering(t,
		filepath.Join(home, ".claude", "projects", "-work-new-foreground", sessionID+".jsonl"),
		"New work", "answer from current cwd")

	s := testServer()
	s.conversationM = conversation.NewReader(home)
	s.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", Agent: "claude", Cwd: newCWD, ForegroundCwd: newForeground, SessionID: sessionID,
	}}, s.state.RevisionCounter())

	if got := s.captureFinishedPane(context.Background(), "pane-1", "claude", oldCWD, sessionID,
		conversation.ProjectContext{CWD: oldCWD, ForegroundCWD: oldForeground}); got != "answer from original cwd" {
		t.Fatalf("captured response = %q, want the transcript bound to the completion's original project", got)
	}
}

func TestLocatedAgentDirUsesTranscriptInsteadOfRawSessionID(t *testing.T) {
	home := t.TempDir()
	profile, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(agentroots.OMPListEnv, profile)
	const sessionID = "123e4567-e89b-12d3-a456-426614174321"
	path := filepath.Join(profile, "sessions", "-work", "session_"+sessionID+".jsonl")
	writeInvariantRows(t, path,
		map[string]any{"type": "message", "message": map[string]any{"role": "user", "content": "question"}})

	reader := conversation.NewReader(home)
	location := reader.LocateWithProject("omp", conversation.ProjectContext{CWD: "/work"}, sessionID)
	if location.Path != path {
		t.Fatalf("location = %#v, want profile transcript %q", location, path)
	}
	if got := locatedAgentDir(home, "omp", location); got != profile {
		t.Fatalf("agent dir = %q, want active profile %q", got, profile)
	}
	if raw := agentroots.AgentDirForSession(home, "omp", sessionID); raw != "" {
		t.Fatalf("raw session ID unexpectedly selected an agent directory: %q", raw)
	}
}

func TestForegroundClaudeTranscriptUsesConfiguredRoot(t *testing.T) {
	home := t.TempDir()
	profile, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(agentroots.ClaudeListEnv, profile)
	const sessionID = "123e4567-e89b-12d3-a456-426614174322"
	path := filepath.Join(profile, "projects", "-work-foreground", sessionID+".jsonl")
	writeInvariantRows(t, path,
		map[string]any{"type": "assistant", "message": map[string]any{"content": "question"}})

	reader := conversation.NewReader(home)
	location := reader.LocateWithProject("claude", conversation.ProjectContext{
		CWD: "/work/pane", ForegroundCWD: "/work/foreground",
	}, sessionID)
	if location.Path != path || location.Root != filepath.Join(profile, "projects") {
		t.Fatalf("location = %#v, want foreground profile transcript %q in configured root", location, path)
	}
}

func TestConversationHistoryCommandFollowsClaudeContinuation(t *testing.T) {
	home := t.TempDir()
	anchor := "123e4567-e89b-12d3-a456-426614174000"
	child := "123e4567-e89b-12d3-a456-426614174001"
	root := filepath.Join(home, ".claude", "projects", "-work-foreground")
	writeInvariantRows(t, filepath.Join(root, anchor+".jsonl"),
		map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{"content": []any{
			map[string]any{"type": "text", "text": "parent response"},
			map[string]any{"type": "tool_use", "id": "parent-edit", "name": "Write", "input": map[string]any{"file_path": "parent.go", "content": "parent"}},
		}}},
		map[string]any{"type": "continued-in", "sessionId": anchor, "continuedInSessionId": child},
	)
	writeInvariantRows(t, filepath.Join(root, child+".jsonl"),
		map[string]any{"type": "assistant", "uuid": "b1", "message": map[string]any{"content": []any{
			map[string]any{"type": "text", "text": "new child response"},
			map[string]any{"type": "tool_use", "id": "edit-1", "name": "Edit", "input": map[string]any{"file_path": "a.go", "old_string": "old", "new_string": "new"}},
		}}},
		map[string]any{"type": "user", "uuid": "result", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "edit-1", "content": "failed to save", "is_error": true, "details": "private-result-marker"},
		}}},
	)
	server := testServer()
	if server.conversationB != nil {
		_ = server.conversationB.Close()
	}
	reader := conversation.NewReader(home)
	browser := newTestBrowser(t, reader, conversation.DefaultBrowserOptions())
	server.conversationM = reader
	server.conversationB = browser
	server.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", Agent: "claude", Cwd: "/work/pane", ForegroundCwd: "/work/foreground", SessionID: anchor,
		TerminalID: "terminal-1", ServerSessionID: "primary", Status: "working",
	}}, server.state.RevisionCounter())
	useProductionHandler(t, server)
	t.Cleanup(func() { browser.Close() })
	conn := dialHub(t, server)
	payload, err := json.Marshal(map[string]any{
		"type": "get_conversation_history", "request_id": "history-1", "pane_id": "pane-1", "limit": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
		t.Fatal(err)
	}
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		OK   bool                    `json:"ok"`
		Data conversation.BrowsePage `json:"data"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		t.Fatal(err)
	}
	if !response.OK || len(response.Data.Entries) != 1 || response.Data.Entries[0].Text != "new child response" || response.Data.NextCursor == "" {
		t.Fatalf("history command response = %s", data)
	}

	tool := response.Data.Entries[0].Tools[0]
	if tool.ID != "edit-1" || tool.Diff == nil || tool.Diff.Text != "-old\n+new" || !tool.Error || tool.Output != "failed to save" || strings.Contains(string(data), "private-result-marker") {
		t.Fatalf("recorded edit response = %s", data)
	}
	source, err := browser.ReadPage(ctx, conversation.BrowseRequest{Scope: conversation.BrowseScope{Provider: "claude", CWD: "/work/pane", ForegroundCWD: "/work/foreground", SessionID: anchor}, Limit: 1})
	if err != nil || len(source.Entries) != 1 || source.Entries[0].Tools[0].Diff != nil {
		t.Fatalf("source page = %+v, error = %v", source, err)
	}

	olderPayload, err := json.Marshal(map[string]any{
		"type": "get_conversation_history", "request_id": "history-2", "pane_id": "pane-1",
		"cursor": response.Data.NextCursor, "limit": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageText, olderPayload); err != nil {
		t.Fatal(err)
	}
	if _, data, err = conn.Read(ctx); err != nil {
		t.Fatal(err)
	}
	var older struct {
		OK   bool                    `json:"ok"`
		Data conversation.BrowsePage `json:"data"`
	}
	if err := json.Unmarshal(data, &older); err != nil {
		t.Fatal(err)
	}
	if !older.OK || len(older.Data.Entries) != 1 || older.Data.Entries[0].Text != "parent response" || len(older.Data.Entries[0].Tools) != 1 {
		t.Fatalf("older history command response = %s", data)
	}
	tool = older.Data.Entries[0].Tools[0]
	if tool.ID != "parent-edit" || tool.Diff == nil || tool.Diff.Path != "parent.go" || tool.Diff.Text != "+parent" {
		t.Fatalf("older edit response = %s", data)
	}
}

func TestConversationHistoryRejectsForegroundChangeDuringRead(t *testing.T) {
	home := t.TempDir()
	const sessionID = "123e4567-e89b-12d3-a456-426614174323"
	oldCWD := "/work/pane"
	oldForeground := "/work/foreground"
	newForeground := "/work/other-foreground"
	writeInvariantRows(t, filepath.Join(home, ".claude", "projects", "-work-foreground", sessionID+".jsonl"),
		map[string]any{"type": "assistant", "uuid": "answer", "message": map[string]any{"content": "foreground answer"}})

	server := testServer()
	if server.conversationB != nil {
		_ = server.conversationB.Close()
	}
	reader := conversation.NewReader(home)
	browser := newTestBrowser(t, reader, conversation.DefaultBrowserOptions())
	server.conversationM = reader
	server.conversationB = browser
	server.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", Agent: "claude", Cwd: oldCWD, ForegroundCwd: oldForeground, SessionID: sessionID,
		TerminalID: "terminal-1", ServerSessionID: "primary", Status: "working",
	}}, server.state.RevisionCounter())

	readEntered := make(chan struct{})
	releaseRead := make(chan struct{})
	server.conversationHistoryReadObserver = func() {
		close(readEntered)
		<-releaseRead
	}
	useProductionHandler(t, server)
	t.Cleanup(func() { browser.Close() })
	conn := dialHub(t, server)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	payload, err := json.Marshal(map[string]any{
		"type": "get_conversation_history", "request_id": "history-stale", "pane_id": "pane-1", "limit": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
		t.Fatal(err)
	}
	select {
	case <-readEntered:
	case <-ctx.Done():
		t.Fatal("history read did not reach the deterministic barrier")
	}

	server.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", Agent: "claude", Cwd: oldCWD, ForegroundCwd: newForeground, SessionID: sessionID,
		TerminalID: "terminal-1", ServerSessionID: "primary", Status: "working",
	}}, server.state.RevisionCounter())
	close(releaseRead)
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		t.Fatal(err)
	}
	if response.OK || response.Error != "Agent changed while conversation history was loading" {
		t.Fatalf("stale history response = %s", data)
	}
}

func TestLatestConversationResponseFollowsClaudeContinuation(t *testing.T) {
	home := t.TempDir()
	anchor := "123e4567-e89b-12d3-a456-426614174000"
	child := "123e4567-e89b-12d3-a456-426614174001"
	root := filepath.Join(home, ".claude", "projects", "-work")
	writeInvariantRows(t, filepath.Join(root, anchor+".jsonl"),
		map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{"content": "parent response"}},
		map[string]any{"type": "continued-in", "sessionId": anchor, "continuedInSessionId": child},
	)
	writeInvariantRows(t, filepath.Join(root, child+".jsonl"),
		map[string]any{"type": "assistant", "uuid": "b1", "message": map[string]any{"content": "new child response"}},
	)
	server := testServer()
	server.conversationM = conversation.NewReader(home)
	if got := server.latestConversationResponse("claude", anchor, conversation.ProjectContext{CWD: "/work"}); got != "new child response" {
		t.Fatalf("latest conversation response = %q, want child response", got)
	}
}

func TestCaptureFinishedPaneFollowsClaudeContinuation(t *testing.T) {
	home := t.TempDir()
	anchor := "123e4567-e89b-12d3-a456-426614174000"
	child := "123e4567-e89b-12d3-a456-426614174001"
	root := filepath.Join(home, ".claude", "projects", "-work")
	writeInvariantRows(t, filepath.Join(root, anchor+".jsonl"),
		map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{"content": "stale parent response"}},
		map[string]any{"type": "continued-in", "sessionId": anchor, "continuedInSessionId": child},
	)
	writeInvariantRows(t, filepath.Join(root, child+".jsonl"),
		map[string]any{"type": "assistant", "uuid": "b1", "message": map[string]any{"content": "finished child response"}},
	)
	server := testServer()
	server.conversationM = conversation.NewReader(home)
	if got := server.captureFinishedPane(context.Background(), "pane-1", "claude", "/work", anchor, conversation.ProjectContext{CWD: "/work"}); got != "finished child response" {
		t.Fatalf("finished-pane extraction = %q, want child response", got)
	}
}

func TestConversationTupleIncludesAgentCwdAndSession(t *testing.T) {
	base := &coordinator.AgentState{Agent: "claude", Cwd: "/work", SessionID: "session"}
	if !sameConversationTuple(base, &coordinator.AgentState{Agent: "claude", Cwd: "/work", SessionID: "session"}) {
		t.Fatal("identical conversation tuples did not match")
	}
	for name, changed := range map[string]*coordinator.AgentState{
		"agent":      {Agent: "qoder", Cwd: "/work", SessionID: "session"},
		"cwd":        {Agent: "claude", Cwd: "/other", SessionID: "session"},
		"foreground": {Agent: "claude", Cwd: "/work", ForegroundCwd: "/work/tree", SessionID: "session"},
		"session":    {Agent: "claude", Cwd: "/work", SessionID: "other"},
	} {
		if sameConversationTuple(base, changed) {
			t.Errorf("%s change was not detected", name)
		}
	}
}

func TestBackgroundClaudeHistoryCaptureDoesNotRequirePhoneRead(t *testing.T) {
	root := t.TempDir()
	fakeHerdr := filepath.Join(root, "herdr")
	script := "#!/bin/sh\nprintf 'first output\\nsecond output\\nfooter 1\\nfooter 2\\nfooter 3\\nfooter 4\\nfooter 5\\nfooter 6\\n'\n"
	if err := os.WriteFile(fakeHerdr, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := exec.Command(fakeHerdr, "--version").Output(); err != nil {
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
	deadline := time.Now().Add(5 * time.Second)
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
