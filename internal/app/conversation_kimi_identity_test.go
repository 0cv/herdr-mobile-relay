package app

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/conversation"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
	"github.com/coder/websocket"
)

func kimiIdentityServer(t *testing.T) (*Server, *coordinator.AgentState, string) {
	t.Helper()
	home := t.TempDir()
	wire := appKimiFixture(t, home, "session_old")
	older := appKimiFixture(t, home, "session_older")
	if err := os.WriteFile(filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(older))), "state.json"), []byte(`{"id":"session_older","version":2,"cwd":"/fixture","updatedAt":500,"archived":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	server := testServerWithCacheDir(t.TempDir())
	server.conversationB.Close()
	server.herdrC.Close()
	server.conversationM = conversation.NewReader(home)
	browser, err := conversation.NewBrowser(server.conversationM, t.TempDir(), conversation.DefaultBrowserOptions())
	if err != nil {
		t.Fatal(err)
	}
	server.conversationB = browser
	server.kimiProcessBirth = func(int) (string, error) { return "synthetic-start-1", nil }
	socketDir, err := os.MkdirTemp("", "kimi-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(socketDir); err != nil {
			t.Error(err)
		}
	})
	socket := filepath.Join(socketDir, "s")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(time.Second))
				var request struct {
					ID     string `json:"id"`
					Params struct {
						Pane string `json:"pane_id"`
					} `json:"params"`
				}
				if json.NewDecoder(conn).Decode(&request) != nil {
					return
				}
				if strings.HasPrefix(request.Params.Pane, "second_failure") {
					json.NewEncoder(conn).Encode(map[string]any{"id": request.ID, "error": map[string]any{"code": -32000, "message": "process info unavailable"}})
					return
				}
				cwd := "/fixture"
				if strings.HasPrefix(request.Params.Pane, "second_departed") {
					cwd = "/elsewhere"
				}
				info := herdr.PaneProcessInfo{PaneID: request.Params.Pane, ShellPID: 10, ForegroundProcessGroupID: 20, ForegroundProcesses: []herdr.PaneProcess{{PID: 20, Cwd: cwd, Argv: []string{"kimi"}}}}
				json.NewEncoder(conn).Encode(map[string]any{"id": request.ID, "result": map[string]any{"type": "pane_process_info", "process_info": info}})
			}()
		}
	}()
	server.herdrC = herdr.NewClient("must-not-run-herdr", socket)
	t.Cleanup(func() { server.herdrC.Close(); listener.Close(); <-done; wg.Wait(); browser.Close() })
	agents := []*coordinator.AgentState{{PaneID: "pane", TerminalID: "terminal", ServerSessionID: "primary", Agent: "kimi", Cwd: "/fixture", Status: "working"}}
	server.enrichKimiSessions(t.Context(), agents)
	server.resolveAgentSessionName(agents[0])
	server.state.CommitInventory(agents, server.state.RevisionCounter())
	agent, _ := server.state.Agent("pane")
	if agent.SessionID != "session_old" || agent.ConversationProcess == "" || !agent.ConversationInferred || len(agent.ConversationInferredOwner) != 64 {
		t.Fatalf("selection=%+v", agent)
	}
	return server, agent, wire
}

func kimiIdentityFeed(t *testing.T, server *Server, agent *coordinator.AgentState) (*conversationFeed, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	feed := &conversationFeed{ctx: ctx, cancel: cancel, agent: agent, target: protocol.TargetRef{PaneID: agent.PaneID, TerminalID: agent.TerminalID, ServerSessionID: agent.ServerSessionID, AgentSessionID: agent.SessionID, Generation: agent.Generation}, ready: make(chan struct{}), notify: make(chan struct{})}
	done := make(chan struct{})
	go func() { defer close(done); server.runConversationFeed(feed) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-feed.ready:
	case <-time.After(time.Second):
		t.Fatal("feed did not start")
	}
	return feed, done
}

func changeKimiIdentity(t *testing.T, server *Server, agent *coordinator.AgentState, wire, change string) {
	t.Helper()
	dir := filepath.Dir(filepath.Dir(filepath.Dir(wire)))
	switch change {
	case "switch":
		next := appKimiFixture(t, filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(dir)))), "session_new")
		if err := os.WriteFile(filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(next))), "state.json"), []byte(`{"id":"session_new","version":2,"cwd":"/fixture","updatedAt":2000}`), 0600); err != nil {
			t.Fatal(err)
		}
	case "resume":
		other := filepath.Join(filepath.Dir(dir), "session_older")
		if err := os.MkdirAll(filepath.Join(other, "agents", "main"), 0700); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(wire)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(other, "agents", "main", "wire.jsonl"), data, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(other, "state.json"), []byte(`{"id":"session_older","version":2,"cwd":"/fixture","updatedAt":2000}`), 0600); err != nil {
			t.Fatal(err)
		}
	case "second_pane", "second_empty", "second_failure", "second_replaced", "second_failure_empty", "second_failure_elsewhere", "second_stale_elsewhere", "second_stale_empty":
		second := *agent
		second.PaneID = change
		second.TerminalID = "second-terminal"
		switch change {
		case "second_empty":
			second.ConversationProcess = ""
		case "second_replaced":
			second.ConversationProcess = "19:old-process"
		case "second_failure_empty", "second_stale_empty":
			second.Cwd, second.ForegroundCwd, second.ConversationProcess = "", "", ""
		case "second_failure_elsewhere", "second_stale_elsewhere":
			second.Cwd, second.ForegroundCwd = "/elsewhere", "/elsewhere"
		}
		server.state.CommitInventory([]*coordinator.AgentState{agent, &second}, server.state.RevisionCounter())
	case "archive":
		if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"id":"session_old","version":2,"cwd":"/fixture","updatedAt":1000,"archived":true}`), 0600); err != nil {
			t.Fatal(err)
		}
	case "delete":
		if err := os.Remove(filepath.Join(dir, "state.json")); err != nil {
			t.Fatal(err)
		}
	case "tombstone":
		root := filepath.Dir(filepath.Dir(filepath.Dir(dir)))
		if err := os.WriteFile(filepath.Join(root, "session_index.jsonl"), []byte("{\"sessionId\":\"session_old\",\"deleted\":true}\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestKimiInferredIdentityLiveFence(t *testing.T) {
	for _, change := range []string{"switch", "resume", "second_pane", "second_empty", "second_failure", "second_replaced", "second_failure_empty", "second_failure_elsewhere", "second_stale_elsewhere", "second_stale_empty", "archive", "delete", "tombstone"} {
		t.Run(change, func(t *testing.T) {
			server, agent, wire := kimiIdentityServer(t)
			feed, done := kimiIdentityFeed(t, server, agent)
			changeKimiIdentity(t, server, agent, wire, change)
			at := time.Now()
			appendFeedRecord(t, wire, kimiAppMessage("assistant", "MUST_NOT_PUBLISH_OLD_SESSION"))
			select {
			case <-done:
			case <-time.After(600 * time.Millisecond):
				t.Fatal("old inferred feed survived selection change")
			}
			feed.mu.Lock()
			batches := len(feed.batches)
			feed.mu.Unlock()
			if batches != 0 || server.conversationCurrent(feed) {
				t.Fatal("old subscription retained data")
			}
			t.Logf("fence latency %s", time.Since(at))
			fresh := &coordinator.AgentState{PaneID: agent.PaneID, TerminalID: agent.TerminalID, Agent: "kimi", Cwd: "/fixture", Status: "working"}
			agents := []*coordinator.AgentState{fresh}
			if strings.HasPrefix(change, "second_") {
				agents = append(agents, &coordinator.AgentState{PaneID: change, Agent: "kimi", Cwd: "/fixture"})
			}
			server.enrichKimiSessions(t.Context(), agents)
			server.resolveAgentSessionName(fresh)
			switch change {
			case "switch":
				if fresh.SessionID != "session_new" {
					t.Fatalf("new selection=%+v", fresh)
				}
			case "resume":
				if fresh.SessionID != "session_older" {
					t.Fatalf("resumed selection=%+v", fresh)
				}
			case "second_pane", "second_empty", "second_failure", "second_replaced", "second_failure_empty", "second_failure_elsewhere", "second_stale_elsewhere", "second_stale_empty":
				if fresh.ConversationReason != "ambiguous_session" || fresh.SessionID != "" {
					t.Fatalf("ambiguous selection=%+v", fresh)
				}
				server.state.CommitInventory(agents, server.state.RevisionCounter())
				ambiguous, _ := server.state.Agent(agent.PaneID)
				if !server.kimiSelectionCurrent(t.Context(), ambiguous) {
					t.Fatal("unchanged ambiguous selection was invalidated")
				}
				server.state.CommitInventory([]*coordinator.AgentState{ambiguous}, server.state.RevisionCounter())
				if server.kimiSelectionCurrent(t.Context(), ambiguous) {
					t.Fatal("peer departure did not invalidate the ambiguous selection")
				}
				server.state.CommitInventory([]*coordinator.AgentState{agent}, server.state.RevisionCounter())
				if !server.kimiSelectionCurrent(t.Context(), agent) {
					t.Fatal("inference did not recover after the peer left topology")
				}
			default:
				if fresh.SessionID != "" {
					t.Fatalf("unavailable selection=%+v", fresh)
				}
			}
		})
	}
}

func TestKimiInferredIdentityHistoryHandlerFences(t *testing.T) {
	for _, change := range []string{"switch", "resume", "second_pane", "second_empty", "second_failure", "second_replaced", "second_failure_empty", "second_failure_elsewhere", "second_stale_elsewhere", "second_stale_empty", "archive", "delete", "tombstone"} {
		t.Run(change, func(t *testing.T) {
			server, agent, wire := kimiIdentityServer(t)
			page, err := server.conversationB.ReadPage(t.Context(), conversation.BrowseRequest{Scope: conversationScope(agent), Limit: 1})
			if err != nil || page.NextCursor == "" {
				t.Fatal("missing cursor")
			}
			server.hub.SetHandler(func(client *transport.ClientConn, message map[string]any, admitted func()) {
				defer admitted()
				inbound, err := protocol.DecodeMap(message)
				if err != nil {
					t.Error(err)
					return
				}
				server.handleConversationHistory(client, inbound)
			})
			httpServer := httptest.NewServer(http.HandlerFunc(server.hub.HandleWebSocket))
			defer httpServer.Close()
			defer server.hub.Shutdown(context.Background())
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(httpServer.URL, "http"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			changeKimiIdentity(t, server, agent, wire, change)
			target := protocol.TargetRef{PaneID: agent.PaneID, TerminalID: agent.TerminalID, ServerSessionID: agent.ServerSessionID, AgentSessionID: agent.SessionID, Generation: agent.Generation}
			for _, observe := range []bool{false, true} {
				for _, cursor := range []string{"", page.NextCursor} {
					raw, _ := json.Marshal(map[string]any{"type": "get_conversation_history", "pane_id": agent.PaneID, "target": target, "request_id": "stale", "observe": observe, "cursor": cursor, "limit": 1})
					if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
						t.Fatal(err)
					}
					_, raw, err := conn.Read(ctx)
					if err != nil {
						t.Fatal(err)
					}
					var result struct {
						OK   bool                    `json:"ok"`
						Data conversation.BrowsePage `json:"data"`
					}
					if err := json.Unmarshal(raw, &result); err != nil {
						t.Fatal(err)
					}
					if result.OK && (result.Data.Available || len(result.Data.Entries) > 0) {
						t.Fatalf("stale handler served data: %s", raw)
					}
				}
			}
		})
	}
}

func TestKimiInferredIdleFreshnessCost(t *testing.T) {
	server, agent, _ := kimiIdentityServer(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	feed := &conversationFeed{ctx: ctx, agent: agent, target: protocol.TargetRef{PaneID: agent.PaneID, TerminalID: agent.TerminalID, ServerSessionID: agent.ServerSessionID, AgentSessionID: agent.SessionID, Generation: agent.Generation}}
	start := time.Now()
	for range 100 {
		if !server.conversationCurrent(feed) {
			t.Fatal("idle selection changed")
		}
		if _, err := server.conversationM.ChangeToken(t.Context(), conversationScope(agent)); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("full freshness+token ticks=100 average=%s", time.Since(start)/100)
}

func TestKimiReportedIdentityRetainsSelection(t *testing.T) {
	server, agent, wire := kimiIdentityServer(t)
	agent.Session = agent.SessionID
	agent.SessionKind = "id"
	server.resolveKimiSessions([]*coordinator.AgentState{agent})
	if agent.ConversationInferred || agent.ConversationInferredOwner != "" {
		t.Fatal("reported ID marked inferred")
	}
	changeKimiIdentity(t, server, agent, wire, "switch")
	changeKimiIdentity(t, server, agent, wire, "archive")
	page, err := server.conversationB.ReadPage(t.Context(), conversation.BrowseRequest{Scope: conversationScope(agent), Limit: 1})
	if err != nil || !page.Available || len(page.Entries) != 1 {
		t.Fatalf("reported ID changed: %+v %v", page, err)
	}
}

func TestKimiInferredIdentityStaleCursor(t *testing.T) {
	for _, change := range []string{"switch", "resume", "archive", "delete", "tombstone"} {
		t.Run(change, func(t *testing.T) {
			server, agent, wire := kimiIdentityServer(t)
			scope := conversationScope(agent)
			page, err := server.conversationB.ReadPage(t.Context(), conversation.BrowseRequest{Scope: scope, Limit: 1})
			if err != nil || page.NextCursor == "" {
				t.Fatalf("initial page=%+v err=%v", page, err)
			}
			changeKimiIdentity(t, server, agent, wire, change)
			for _, cursor := range []string{"", page.NextCursor} {
				stale, err := server.conversationB.ReadPage(t.Context(), conversation.BrowseRequest{Scope: scope, Cursor: cursor, Limit: 1})
				if err != nil || stale.Available || len(stale.Entries) != 0 {
					t.Fatalf("stale page served: %+v err=%v", stale, err)
				}
			}
		})
	}
}
