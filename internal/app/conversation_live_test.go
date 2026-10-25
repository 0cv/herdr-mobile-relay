package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/conversation"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
)

func TestConversationActivityDoesNotReplaceOtherProviderRefreshPaths(t *testing.T) {
	for _, agent := range []string{"pi", "PiCodingAgent", "Claude_Code", "OpenAI Codex", "cursor", "Cursor-Agent", "cursoragent", "qoder", "Qoder CLI", "qodercli", "omp", "ohmypi", "Oh My Pi", "opencode", "omo", "Oh My OpenCode", "hermes", "Hermes-Agent", "kimi", "Kimi-Code"} {
		if !supportsConversationActivity(agent) {
			t.Fatalf("activity source rejected: %s", agent)
		}
	}
	for _, agent := range []string{"unknown"} {
		if supportsConversationActivity(agent) {
			t.Fatalf("unqualified source must retain its existing refresh path: %s", agent)
		}
	}
}

func TestConversationFeedJournalIsBoundedAndSignalsChanges(t *testing.T) {
	feed := &conversationFeed{ready: make(chan struct{}), notify: make(chan struct{})}
	original := feed.notify
	for sequence := uint64(1); sequence <= 200; sequence++ {
		feed.publish(conversationBatch{Kind: "batch", SourceRevision: "fixture:1", Sequence: sequence, Entries: []conversation.Entry{{ID: "pi:a", Role: "assistant", Text: strings.Repeat("x", 64000)}}})
	}
	select {
	case <-original:
	default:
		t.Fatal("listeners were not notified")
	}
	if feed.bytes > conversationFeedBytes || len(feed.batches) > 128 || feed.batches[0].Sequence <= 1 || feed.sequence != 200 {
		t.Fatalf("bounds: bytes=%d batches=%d sequence=%d", feed.bytes, len(feed.batches), feed.sequence)
	}
	feed.publish(conversationBatch{Kind: "reset", SourceRevision: "fixture:2", Sequence: 1})
	if len(feed.batches) != 1 || feed.batches[0].Remove == nil || feed.batches[0].Entries == nil {
		t.Fatal("source reset must discard replay and normalize empty lists")
	}
}

func TestConversationUnsubscribeCannotCancelReplacementSubscription(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	feed := &conversationFeed{ctx: ctx, cancel: cancel, clients: map[string]time.Time{"client": time.Now()}, subscriptions: map[string]string{"client": "new-subscription"}}
	server := &Server{conversationClients: map[string]*conversationFeed{"client": feed}}
	server.stopConversationFeed("client", "old-subscription")
	if ctx.Err() != nil || server.conversationClients["client"] != feed {
		t.Fatal("stale unsubscribe canceled the replacement")
	}
	server.stopConversationFeed("client", "new-subscription")
	if ctx.Err() == nil || server.conversationClients["client"] != nil {
		t.Fatal("current unsubscribe did not release source")
	}
}

func TestRecordedConversationFeedPublishesOnlyChangedEntries(t *testing.T) {
	home := t.TempDir()
	session := "123e4567-e89b-12d3-a456-426614174000"
	path := filepath.Join(home, ".claude", "projects", "-fixture", session+".jsonl")
	writeInvariantRows(t, path, map[string]any{"type": "user", "uuid": "u1", "message": map[string]any{"content": "fixture prompt"}}, map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{"content": "fixture first"}})
	server := testServer()
	if server.conversationB != nil {
		server.conversationB.Close()
	}
	reader := conversation.NewReader(home)
	browser, err := conversation.NewBrowser(reader, t.TempDir(), conversation.DefaultBrowserOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	server.conversationM = reader
	server.conversationB = browser
	server.state.CommitInventory([]*coordinator.AgentState{{PaneID: "fixture-pane", Agent: "claude", Cwd: "/fixture", SessionID: session, AgentSessionID: session, TerminalID: "fixture-terminal", ServerSessionID: "primary", Status: "working"}}, server.state.RevisionCounter())
	agent, _ := server.state.Agent("fixture-pane")
	target := protocol.TargetRef{PaneID: agent.PaneID, TerminalID: agent.TerminalID, Generation: agent.Generation, ServerSessionID: agent.ServerSessionID, AgentSessionID: session}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	feed := &conversationFeed{ctx: ctx, cancel: cancel, target: target, agent: agent, ready: make(chan struct{}), notify: make(chan struct{})}
	done := make(chan struct{})
	go func() { defer close(done); server.runRecordedConversationFeed(feed) }()
	defer func() { cancel(); <-done }()
	select {
	case <-feed.ready:
	case <-time.After(3 * time.Second):
		t.Fatal("initial source did not load")
	}
	feed.mu.Lock()
	initial := feed.sequence
	caps := feed.capabilities
	feed.mu.Unlock()
	if caps.Source != "recorded_history" || caps.ActiveBranch {
		t.Fatalf("misleading capabilities: %+v", caps)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	err = json.NewEncoder(file).Encode(map[string]any{"type": "assistant", "uuid": "a2", "message": map[string]any{"content": "fixture second"}})
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		feed.mu.Lock()
		sequence := feed.sequence
		var batch conversationBatch
		if len(feed.batches) > 0 {
			batch = feed.batches[len(feed.batches)-1]
		}
		feed.mu.Unlock()
		if sequence > initial {
			if batch.Kind != "batch" || len(batch.Entries) != 1 || batch.Entries[0].Text != "fixture second" {
				t.Fatalf("delta=%+v", batch)
			}
			time.Sleep(300 * time.Millisecond)
			feed.mu.Lock()
			unchanged := feed.sequence == sequence
			feed.mu.Unlock()
			if !unchanged {
				t.Fatal("unchanged source produced updates")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("appended record did not publish")
}

func TestRecordedConversationFeedContinuationAndDiagnostics(t *testing.T) {
	for _, scenario := range []string{"descendant", "corrupt", "missing_child", "availability", "nonready"} {
		t.Run(scenario, func(t *testing.T) {
			home := t.TempDir()
			session := "123e4567-e89b-12d3-a456-426614174000"
			child := "123e4567-e89b-12d3-a456-426614174001"
			provider := "claude"
			path := filepath.Join(home, ".claude", "projects", "-fixture", session+".jsonl")
			if scenario == "corrupt" {
				provider = "codex"
				path = filepath.Join(home, ".codex", "sessions", "2026", "10", "18", "rollout-"+session+".jsonl")
				writeInvariantRows(t, path, map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "assistant", "content": []map[string]any{{"type": "output_text", "text": "seed"}}}})
			} else {
				writeInvariantRows(t, path, map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{"content": "seed"}})
			}
			childPath := filepath.Join(filepath.Dir(path), child+".jsonl")
			if scenario == "descendant" {
				writeInvariantRows(t, childPath, map[string]any{"type": "assistant", "uuid": "b1", "message": map[string]any{"content": "child seed"}})
				appendFeedRecord(t, path, map[string]any{"type": "continued-in", "continuedInSessionId": child})
			}
			server := testServerWithCacheDir(t.TempDir())
			server.conversationB.Close()
			reader := conversation.NewReader(home)
			browser, err := conversation.NewBrowser(reader, t.TempDir(), conversation.DefaultBrowserOptions())
			if err != nil {
				t.Fatal(err)
			}
			defer browser.Close()
			if scenario == "nonready" {
				browser.Close()
			}
			defer server.herdrC.Close()
			server.conversationM, server.conversationB = reader, browser
			server.state.CommitInventory([]*coordinator.AgentState{{PaneID: "fixture-pane", Agent: provider, Cwd: "/fixture", SessionID: session, AgentSessionID: session, TerminalID: "fixture-terminal", ServerSessionID: "primary", Status: "working"}}, server.state.RevisionCounter())
			agent, _ := server.state.Agent("fixture-pane")
			target := protocol.TargetRef{PaneID: agent.PaneID, TerminalID: agent.TerminalID, Generation: agent.Generation, ServerSessionID: agent.ServerSessionID, AgentSessionID: session}
			ctx, cancel := context.WithCancel(t.Context())
			feed := &conversationFeed{ctx: ctx, cancel: cancel, target: target, agent: agent, ready: make(chan struct{}), notify: make(chan struct{})}
			done := make(chan struct{})
			go func() { defer close(done); server.runRecordedConversationFeed(feed) }()
			defer func() { cancel(); <-done }()
			select {
			case <-feed.ready:
			case <-time.After(3 * time.Second):
				t.Fatal("initial feed timed out")
			}
			feed.mu.Lock()
			initial := feed.sequence
			feed.mu.Unlock()
			if scenario == "nonready" {
				page, err := browser.ReadPage(ctx, conversation.BrowseRequest{Scope: conversationScope(agent), Limit: 200})
				if err != nil || page.State != conversation.BrowseFailed {
					t.Fatalf("expected failed browser: %+v, err=%v", page, err)
				}
				time.Sleep(800 * time.Millisecond)
				feed.mu.Lock()
				sequence, batches := feed.sequence, len(feed.batches)
				feed.mu.Unlock()
				if sequence != initial || batches != 1 {
					t.Fatalf("unchanged non-ready polls published duplicates: sequence=%d want=%d batches=%d", sequence, initial, batches)
				}
				return
			}
			switch scenario {
			case "descendant":
				appendFeedRecord(t, childPath, map[string]any{"type": "assistant", "uuid": "b2", "message": map[string]any{"content": "child append"}})
			case "missing_child":
				appendFeedRecord(t, path, map[string]any{"type": "continued-in", "continuedInSessionId": child})
			case "corrupt":
				f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				_, err = f.WriteString("{invalid-json}\n")
				f.Close()
				if err != nil {
					t.Fatal(err)
				}
			case "availability":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			batch := waitFeedBatch(t, feed, initial)
			if scenario == "descendant" {
				if batch.Kind != "batch" || len(batch.Entries) != 1 || batch.Entries[0].Text != "child append" {
					t.Fatalf("descendant delta: %+v", batch)
				}
			} else if batch.Kind != "reset" {
				t.Fatalf("expected resnapshot for %s: %+v", scenario, batch)
			}
			page, err := browser.ReadPage(ctx, conversation.BrowseRequest{Scope: conversationScope(agent), Limit: 200})
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "corrupt" && page.Diagnostics.CorruptRecords != 1 {
				t.Fatalf("diagnostics: %+v", page.Diagnostics)
			}
			if scenario == "availability" {
				if page.Available {
					t.Fatalf("removed source remained ready: %+v", page)
				}
				feed.mu.Lock()
				initialBatches := len(feed.batches)
				feed.mu.Unlock()
				time.Sleep(800 * time.Millisecond)
				feed.mu.Lock()
				sequence, batches := feed.sequence, len(feed.batches)
				feed.mu.Unlock()
				if sequence != batch.Sequence || batches != initialBatches {
					t.Fatalf("unchanged unavailable polls published duplicates: sequence=%d want=%d batches=%d", sequence, batch.Sequence, batches)
				}
				writeInvariantRows(t, path, map[string]any{"type": "assistant", "uuid": "a2", "message": map[string]any{"content": "recovered"}})
				if recovery := waitFeedBatch(t, feed, sequence); recovery.Kind != "reset" {
					t.Fatalf("availability recovery: %+v", recovery)
				}
				page, err = browser.ReadPage(ctx, conversation.BrowseRequest{Scope: conversationScope(agent), Limit: 200})
				if err != nil || page.State != conversation.BrowseReady || !page.Available || len(page.Entries) != 1 || page.Entries[0].Text != "recovered" {
					t.Fatalf("recovery page: %+v, err=%v", page, err)
				}
			}
			if scenario == "missing_child" {
				if !page.Diagnostics.ContinuationIncomplete {
					t.Fatalf("diagnostics: %+v", page.Diagnostics)
				}
				writeInvariantRows(t, childPath, map[string]any{"type": "assistant", "uuid": "b1", "message": map[string]any{"content": "child appeared"}})
				if recovery := waitFeedBatch(t, feed, batch.Sequence); recovery.Kind != "reset" {
					t.Fatalf("diagnostic recovery: %+v", recovery)
				}
			}
		})
	}
}

func appendFeedRecord(t *testing.T, path string, record any) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	err = json.NewEncoder(f).Encode(record)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
}

func waitFeedBatch(t *testing.T, feed *conversationFeed, after uint64) conversationBatch {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		feed.mu.Lock()
		for _, batch := range feed.batches {
			if batch.Sequence > after {
				feed.mu.Unlock()
				return batch
			}
		}
		feed.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("changed source did not wake feed")
	return conversationBatch{}
}
