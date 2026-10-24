package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
	"github.com/0cv/herdr-mobile-relay/internal/conversation"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
	"github.com/coder/websocket"
)

func TestDatabaseLoadedHistoryEditDelivery(t *testing.T) {
	for _, format := range []string{"v1", "v2", "mixed-v1", "mixed-v2", "hermes"} {
		t.Run(format, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv(agentroots.OpenCodeListEnv, root)
			t.Setenv(agentroots.HermesListEnv, root)
			t.Setenv("HERMES_HOME", "")
			provider, session, name := "opencode", "ses_loaded123", "opencode.db"
			if format == "hermes" {
				provider, session, name = "hermes", "loaded-hermes", "state.db"
			}
			db, err := sql.Open("sqlite", filepath.Join(root, name))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			exec := func(query string, args ...any) {
				t.Helper()
				if _, err := db.Exec(query, args...); err != nil {
					t.Fatal(err)
				}
			}
			exec("PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0")
			v2 := format == "v2" || format == "mixed-v2"
			if format == "hermes" {
				exec(`CREATE TABLE sessions(id TEXT PRIMARY KEY,cwd TEXT,title TEXT); CREATE TABLE messages(id INTEGER PRIMARY KEY,session_id TEXT,role TEXT,content TEXT,tool_call_id TEXT,tool_calls TEXT,tool_name TEXT,timestamp REAL,active INTEGER,compacted INTEGER,display_kind TEXT); INSERT INTO sessions VALUES(?,?,'Live')`, session, root)
			} else {
				if format != "v2" {
					exec(`CREATE TABLE session(id TEXT PRIMARY KEY,directory TEXT,title TEXT,time_updated INTEGER,agent TEXT); CREATE TABLE message(id TEXT PRIMARY KEY,session_id TEXT,time_created INTEGER,data TEXT); CREATE INDEX message_order ON message(session_id,time_created,id); CREATE TABLE part(id TEXT PRIMARY KEY,message_id TEXT,data TEXT); CREATE INDEX part_message ON part(message_id); INSERT INTO session VALUES(?,?,'Live',1,'opencode')`, session, root)
				}
				if format != "v1" {
					exec(`CREATE TABLE session_v2(id TEXT PRIMARY KEY,directory TEXT,title TEXT,time_updated INTEGER,agent TEXT); CREATE TABLE session_message(id TEXT PRIMARY KEY,session_id TEXT,type TEXT,seq INTEGER,time_created INTEGER,data TEXT); CREATE INDEX session_order ON session_message(session_id,seq); INSERT INTO session_v2 VALUES(?,?,'Live',1,'opencode')`, session, root)
				}
			}
			for i := 1; i <= 320; i++ {
				id := fmt.Sprintf("m%04d", i)
				switch {
				case format == "hermes":
					exec(`INSERT INTO messages VALUES(?,?,'assistant',?,NULL,NULL,NULL,?,1,0,'')`, i, session, fmt.Sprintf("seed %d", i), i)
				case v2:
					exec(`INSERT INTO session_message VALUES(?,?,'assistant',?,?,?)`, id, session, i, i, fmt.Sprintf(`{"content":[{"type":"text","text":"seed %d"}]}`, i))
				default:
					exec(`INSERT INTO message VALUES(?,?,?,'{"role":"assistant"}')`, id, session, i)
					exec(`INSERT INTO part VALUES(?,?,?)`, "p"+id, id, fmt.Sprintf(`{"type":"text","text":"seed %d"}`, i))
				}
			}
			server := testServerWithCacheDir(t.TempDir())
			server.conversationB.Close()
			reader := conversation.NewReader(t.TempDir())
			browser, err := conversation.NewBrowser(reader, t.TempDir(), conversation.DefaultBrowserOptions())
			if err != nil {
				t.Fatal(err)
			}
			defer browser.Close()
			defer server.herdrC.Close()
			server.conversationM, server.conversationB = reader, browser
			defer func() {
				server.conversationLiveMu.Lock()
				feeds := make([]*conversationFeed, 0, len(server.conversationFeeds))
				for _, feed := range server.conversationFeeds {
					feed.cancel()
					feeds = append(feeds, feed)
				}
				server.conversationLiveMu.Unlock()
				stopped, stop := context.WithTimeout(t.Context(), 5*time.Second)
				defer stop()
				for _, feed := range feeds {
					for {
						feed.mu.Lock()
						finished, notify := feed.failure, feed.notify
						feed.mu.Unlock()
						if finished {
							break
						}
						select {
						case <-notify:
						case <-stopped.Done():
							t.Error("database fixture feed did not stop")
							return
						}
					}
				}
			}()
			server.state.CommitInventory([]*coordinator.AgentState{{PaneID: "loaded-pane", TerminalID: "loaded-terminal", ServerSessionID: "primary", Agent: provider, Cwd: root, SessionID: session, AgentSessionID: session, Status: "working"}}, server.state.RevisionCounter())
			agent, _ := server.state.Agent("loaded-pane")
			target := protocol.TargetRef{PaneID: agent.PaneID, TerminalID: agent.TerminalID, ServerSessionID: agent.ServerSessionID, AgentSessionID: session, Generation: agent.Generation}
			server.hub.SetHandler(func(client *transport.ClientConn, message map[string]any, admitted func()) {
				defer admitted()
				inbound, err := protocol.DecodeMap(message)
				if err != nil {
					t.Error(err)
					return
				}
				if inbound.Type == "get_conversation_updates" {
					server.handleConversationUpdates(client, inbound)
					return
				}
				server.handleConversationHistory(client, inbound)
			})
			server.hub.SetOnDisconnect(func(client *transport.ClientConn) { server.stopConversationFeed(client.ID(), "") })
			httpServer := httptest.NewServer(http.HandlerFunc(server.hub.HandleWebSocket))
			defer httpServer.Close()
			defer server.hub.Shutdown(context.Background())
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(httpServer.URL, "http"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			request := func(message map[string]any, result any) {
				t.Helper()
				message["pane_id"], message["target"], message["request_id"] = agent.PaneID, target, "loaded-request"
				raw, _ := json.Marshal(message)
				if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
					t.Fatal(err)
				}
				_, raw, err = conn.Read(ctx)
				if err != nil {
					t.Fatal(err)
				}
				var response struct {
					OK   bool            `json:"ok"`
					Data json.RawMessage `json:"data"`
				}
				if json.Unmarshal(raw, &response) != nil || !response.OK {
					t.Fatalf("request failed: %s", raw)
				}
				if err := json.Unmarshal(response.Data, result); err != nil {
					t.Fatal(err)
				}
			}
			var page, older conversation.BrowsePage
			request(map[string]any{"type": "get_conversation_history", "observe": true, "limit": 200}, &page)
			request(map[string]any{"type": "get_conversation_history", "observe": true, "limit": 200, "cursor": page.NextCursor}, &older)
			if len(older.Entries) != 120 {
				t.Fatalf("older page=%+v", older)
			}
			id := older.Entries[0].ID
			switch {
			case format == "hermes":
				exec(`UPDATE messages SET content='loaded edit' WHERE id=?`, id)
			case v2:
				exec(`UPDATE session_message SET data='{"content":[{"type":"text","text":"loaded edit"}]}' WHERE id=?`, id)
			default:
				exec(`UPDATE part SET data='{"type":"text","text":"loaded edit"}' WHERE message_id=?`, id)
			}
			var batch conversationBatch
			start := time.Now()
			request(map[string]any{"type": "get_conversation_updates", "subscription_id": older.SubscriptionID, "source_revision": older.SourceRevision, "after": older.Sequence}, &batch)
			if batch.Kind != "batch" || len(batch.Entries) != 1 || batch.Entries[0].ID != id || batch.Entries[0].Text != "loaded edit" {
				t.Fatalf("loaded edit not delivered: %+v", batch)
			}
			t.Logf("format=%s loaded=320 older_edit_to_batch_ms=%.3f", format, float64(time.Since(start))/float64(time.Millisecond))
		})
	}
}

func TestDatabaseBatchKeepsViewerWindowsSeparate(t *testing.T) {
	feed := &conversationFeed{database: &conversation.DatabaseWatch{}, loaded: map[string]map[string]bool{"older": {"old": true}, "recent": {}}}
	batch := conversationBatch{Kind: "batch", Entries: []conversation.Entry{{ID: "old", Text: "edited"}, {ID: "new", Text: "appended"}}, recordedEdits: map[string]bool{"old": true}}
	older := feed.databaseBatchLocked("older", batch)
	recent := feed.databaseBatchLocked("recent", batch)
	if len(older.Entries) != 2 || len(recent.Entries) != 1 || recent.Entries[0].ID != "new" || len(batch.Entries) != 2 {
		t.Fatalf("viewer windows leaked or replay mutated: older=%+v recent=%+v replay=%+v", older, recent, batch)
	}
	updated := feed.databaseBatchLocked("recent", conversationBatch{Kind: "batch", Entries: []conversation.Entry{{ID: "new", Text: "edited append"}}, recordedEdits: map[string]bool{"new": true}})
	if len(updated.Entries) != 1 {
		t.Fatal("delivered append was not registered as loaded")
	}
	feed.databaseBatchLocked("recent", conversationBatch{Kind: "reset"})
	if len(feed.loaded["recent"]) != 0 {
		t.Fatal("reset retained viewer window")
	}
}
