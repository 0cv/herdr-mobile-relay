package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/conversation"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
)

func appKimiFixture(t *testing.T, home, id string) string {
	t.Helper()
	hash := sha256.Sum256([]byte("/fixture"))
	dir := filepath.Join(home, ".kimi-code", "sessions", fmt.Sprintf("wd_fixture_%x", hash[:6]), id)
	path := filepath.Join(dir, "agents", "main", "wire.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	state, _ := json.Marshal(map[string]any{"id": id, "version": 2, "cwd": "/fixture", "updatedAt": 1000, "archived": false, "title": "Kimi fixture"})
	if err := os.WriteFile(filepath.Join(dir, "state.json"), state, 0600); err != nil {
		t.Fatal(err)
	}
	writeInvariantRows(t, path, map[string]any{"type": "metadata", "protocol_version": "1.5"}, kimiAppMessage("user", "Seed prompt"), kimiAppMessage("assistant", "Seed answer"))
	return path
}

func kimiAppMessage(role, text string) map[string]any {
	return map[string]any{"type": "context.append_message", "message": map[string]any{"role": role, "content": []map[string]any{{"type": "text", "text": text}}}}
}

func TestKimiForegroundProcessFence(t *testing.T) {
	info := &herdr.PaneProcessInfo{PaneID: "pane", ShellPID: 10, ForegroundProcessGroupID: 20, ForegroundProcesses: []herdr.PaneProcess{{PID: 20, Cwd: "/fixture", Argv: []string{"kimi", "--continue"}}}}
	first, cwd, err := kimiProcessIdentity(info, "pane")
	if err != nil || cwd != "/fixture" || first == "" {
		t.Fatal("missing foreground identity")
	}
	for _, change := range []func(){func() { info.ForegroundProcesses[0].Cwd = "/other" }, func() { info.ForegroundProcesses[0].Argv = []string{"kimi", "--session", "id"} }, func() { info.ForegroundProcesses[0].PID = 21; info.ForegroundProcessGroupID = 21 }} {
		change()
		next, _, err := kimiProcessIdentity(info, "pane")
		if err != nil || next == first {
			t.Fatal("process/cwd/argv change not fenced")
		}
		first = next
	}
	if _, _, err := kimiProcessIdentity(info, "other-pane"); err == nil {
		t.Fatal("pane mismatch accepted")
	}
	info.ForegroundProcesses = append(info.ForegroundProcesses, herdr.PaneProcess{PID: 22, Cwd: "/fixture"})
	if _, _, err := kimiProcessIdentity(info, "pane"); err == nil {
		t.Fatal("ambiguous foreground process accepted")
	}
}

func TestKimiSessionIdentityPolicyAndProcessGeneration(t *testing.T) {
	home := t.TempDir()
	appKimiFixture(t, home, "session_identity")
	server := testServerWithCacheDir(t.TempDir())
	defer server.herdrC.Close()
	defer server.conversationB.Close()
	server.conversationM = conversation.NewReader(home)
	one := &coordinator.AgentState{PaneID: "one", Agent: "kimi", Cwd: "/wrong", ForegroundCwd: "/fixture", ConversationProcess: "100:argv", TerminalID: "terminal", Status: "working"}
	server.resolveKimiSessions([]*coordinator.AgentState{one})
	server.resolveAgentSessionName(one)
	if one.SessionID != "session_identity" || !one.ConversationHistoryAvailable {
		t.Fatalf("single pane=%+v", one)
	}
	missing := &coordinator.AgentState{PaneID: "two", Agent: "Kimi-Code", Cwd: "/fixture/", Status: "working"}
	reported := &coordinator.AgentState{PaneID: "one", Agent: "kimi", Cwd: "/fixture", Session: "session_identity", SessionKind: "id", Status: "working"}
	server.resolveKimiSessions([]*coordinator.AgentState{missing, reported})
	server.resolveAgentSessionName(missing)
	server.resolveAgentSessionName(reported)
	if missing.ConversationReason != "ambiguous_session" || missing.ConversationHistoryAvailable || reported.SessionID != "session_identity" {
		t.Fatalf("ambiguous=%+v reported=%+v", missing, reported)
	}
	for _, agent := range []*coordinator.AgentState{
		{Agent: "kimi", Cwd: "/fixture", Session: "missing", SessionKind: "id"},
		{Agent: "kimi", Cwd: "/fixture", Session: "session_identity", SessionKind: "path"},
		{Agent: "kimi", Cwd: "/no-session"},
	} {
		server.resolveKimiSessions([]*coordinator.AgentState{agent})
		server.resolveAgentSessionName(agent)
		if agent.ConversationHistoryAvailable || agent.ConversationReason == "" {
			t.Fatalf("unsafe fallback=%+v", agent)
		}
	}
	server.state.CommitInventory([]*coordinator.AgentState{one}, server.state.RevisionCounter())
	before, _ := server.state.Agent("one")
	replaced := *one
	replaced.ConversationProcess = "101:new-argv"
	server.state.CommitInventory([]*coordinator.AgentState{&replaced}, server.state.RevisionCounter())
	after, _ := server.state.Agent("one")
	if after.Generation == before.Generation || sameConversationTuple(before, after) {
		t.Fatal("process change did not fence target")
	}
}

func TestKimiRecordedFeedAppendUndoBranchAndIncompleteResets(t *testing.T) {
	home := t.TempDir()
	id := "session_feed"
	path := appKimiFixture(t, home, id)
	server := testServerWithCacheDir(t.TempDir())
	server.conversationB.Close()
	defer server.herdrC.Close()
	reader := conversation.NewReader(home)
	browser, err := conversation.NewBrowser(reader, t.TempDir(), conversation.DefaultBrowserOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	server.conversationM, server.conversationB = reader, browser
	server.state.CommitInventory([]*coordinator.AgentState{{PaneID: "kimi-pane", TerminalID: "terminal", ServerSessionID: "primary", Agent: "kimi", Cwd: "/fixture", SessionID: id, AgentSessionID: id, Status: "working"}}, server.state.RevisionCounter())
	agent, _ := server.state.Agent("kimi-pane")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	feed := &conversationFeed{ctx: ctx, cancel: cancel, agent: agent, target: protocol.TargetRef{PaneID: agent.PaneID, TerminalID: agent.TerminalID, ServerSessionID: agent.ServerSessionID, AgentSessionID: id, Generation: agent.Generation}, ready: make(chan struct{}), notify: make(chan struct{})}
	done := make(chan struct{})
	go func() { defer close(done); server.runRecordedConversationFeed(feed) }()
	defer func() { cancel(); <-done }()
	var seq uint64
	wait := func(kind, text string) conversationBatch {
		t.Helper()
		deadline := time.NewTimer(5 * time.Second)
		defer deadline.Stop()
		for {
			feed.mu.Lock()
			batches := append([]conversationBatch{}, feed.batches...)
			notify := feed.notify
			feed.mu.Unlock()
			for _, batch := range batches {
				if batch.Sequence > seq {
					seq = batch.Sequence
					raw, _ := json.Marshal(batch)
					if batch.Kind != kind || text != "" && !strings.Contains(string(raw), text) {
						t.Fatalf("batch=%s want=%s %q", raw, kind, text)
					}
					return batch
				}
			}
			select {
			case <-deadline.C:
				t.Fatal("no Kimi update")
			case <-notify:
			}
		}
	}
	initial := wait("batch", "Seed answer")
	if initial.Capabilities == nil || !initial.Capabilities.ActiveBranch {
		t.Fatal("missing validated branch capability")
	}
	appendFeedRecord(t, path, kimiAppMessage("user", "Added prompt"))
	appendFeedRecord(t, path, kimiAppMessage("assistant", "Added answer"))
	appended := wait("batch", "Added answer")
	if appended.SourceRevision != initial.SourceRevision {
		t.Fatal("append reset")
	}
	appendFeedRecord(t, path, map[string]any{"type": "context.undo", "count": 1})
	undo := wait("reset", "")
	if undo.SourceRevision == initial.SourceRevision {
		t.Fatal("undo revision unchanged")
	}
	branchSample, err := os.ReadFile(filepath.Join("..", "conversation", "testdata", "kimi", "v2.2.0-branches", "wire.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, branchSample, 0600); err != nil {
		t.Fatal(err)
	}
	wait("reset", "")
	appendFeedRecord(t, path, kimiAppMessage("assistant", "Branch append"))
	wait("batch", "Branch append")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("{\"type\":\"agent.switched\",\"agentId\":\"main\",\"branch\":\"b3\",\"reason\":\"undo\",\"base\":{\"branch\":\"b2\",\"line\":23},\"turns\":1,\"legacyUndoLine\":26}\n"); err != nil {
		t.Fatal(err)
	}
	file.Close()
	wait("reset", "")
	time.Sleep(650 * time.Millisecond)
	feed.mu.Lock()
	stable := feed.sequence
	feed.mu.Unlock()
	if stable != seq {
		t.Fatal("unavailable history repeatedly reset")
	}
	page, _ := browser.ReadPage(t.Context(), conversation.BrowseRequest{Scope: conversationScope(agent), Limit: 200})
	if page.Available || page.ReasonCode != "source_corrupt" || len(page.Entries) != 0 {
		t.Fatalf("incomplete switch=%+v", page)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = browser.ReadPage(t.Context(), conversation.BrowseRequest{Scope: conversationScope(agent), Limit: 200})
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("reader mutated journal")
	}
}
