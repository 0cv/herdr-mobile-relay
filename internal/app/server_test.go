package app

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
)

func testServer() *Server {
	cfg := &config.Config{
		Host:       "127.0.0.1",
		Port:       8375,
		InstanceID: "test-instance",
	}
	return New(cfg, "0.9.0", "abc123", slog.New(slog.NewTextHandler(io.Discard, nil)))
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

	// Wait for server to be ready
	var resp *http.Response
	var err error
	for i := 0; i < 50; i++ {
		resp, err = http.Get("http://127.0.0.1:18999/health")
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("server did not start: %v", err)
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
