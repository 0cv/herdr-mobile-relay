package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/conversation"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
)

func TestKimiInferredIdentityDuplicateRoot(t *testing.T) {
	server, _, firstWire := kimiIdentityServer(t)
	firstDir := filepath.Dir(filepath.Dir(filepath.Dir(firstWire)))
	firstRoot := filepath.Dir(filepath.Dir(filepath.Dir(firstDir)))
	home := t.TempDir()
	secondWire := appKimiFixture(t, home, "session_old")
	data, err := os.ReadFile(secondWire)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secondWire, []byte(strings.ReplaceAll(string(data), "Seed answer", "Second root answer")), 0600); err != nil {
		t.Fatal(err)
	}
	secondDir := filepath.Dir(filepath.Dir(filepath.Dir(secondWire)))
	if err := os.WriteFile(filepath.Join(secondDir, "state.json"), []byte(`{"id":"session_old","version":2,"cwd":"/fixture","updatedAt":2000,"title":"Second root"}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_KIMI_CODE_HOME_DIRS", firstRoot)
	t.Setenv("KIMI_CODE_HOME", "")
	server.conversationB.Close()
	server.conversationM = conversation.NewReader(home)
	browser, err := conversation.NewBrowser(server.conversationM, t.TempDir(), conversation.DefaultBrowserOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	server.conversationB = browser
	resolve := func() *coordinator.AgentState {
		t.Helper()
		agent := &coordinator.AgentState{PaneID: "pane", TerminalID: "terminal", ServerSessionID: "primary", Agent: "kimi", Cwd: "/fixture", Status: "working"}
		server.enrichKimiSessions(t.Context(), []*coordinator.AgentState{agent})
		server.resolveAgentSessionName(agent)
		server.state.CommitInventory([]*coordinator.AgentState{agent}, server.state.RevisionCounter())
		current, _ := server.state.Agent("pane")
		return current
	}
	before := resolve()
	feed, done := kimiIdentityFeed(t, server, before)
	page, err := browser.ReadPage(t.Context(), conversation.BrowseRequest{Scope: conversationScope(before), Limit: 1})
	if err != nil || page.NextCursor == "" {
		t.Fatalf("initial page=%+v %v", page, err)
	}
	changeKimiIdentity(t, server, before, firstWire, "archive")
	appendFeedRecord(t, firstWire, kimiAppMessage("assistant", "MUST_NOT_PUBLISH_ARCHIVED_ROOT"))
	select {
	case <-done:
	case <-time.After(600 * time.Millisecond):
		t.Fatal("old duplicate-ID feed survived root archive")
	}
	if server.conversationCurrent(feed) {
		t.Fatal("old source still current")
	}
	after := resolve()
	if after.SessionID != before.SessionID || after.ConversationSource == before.ConversationSource || after.Generation != before.Generation+1 || after.SessionName != "Second root" {
		t.Fatalf("root replacement before=%+v after=%+v", before, after)
	}
	fresh, err := browser.ReadPage(t.Context(), conversation.BrowseRequest{Scope: conversationScope(after), Limit: 1})
	if err != nil || !fresh.Available || len(fresh.Entries) != 1 || fresh.Entries[0].Text != "Second root answer" {
		t.Fatalf("replacement read wrong root=%+v %v", fresh, err)
	}
	stale, err := browser.ReadPage(t.Context(), conversation.BrowseRequest{Scope: conversationScope(after), Cursor: page.NextCursor, Limit: 1})
	if err != nil || len(stale.Entries) != 0 || stale.ReasonCode != "invalid_cursor" {
		t.Fatalf("old root cursor survived=%+v %v", stale, err)
	}
}
