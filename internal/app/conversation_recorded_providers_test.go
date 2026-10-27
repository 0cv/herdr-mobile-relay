package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/conversation"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
	"github.com/coder/websocket"
)

func TestRecordedConversationProviderAliases(t *testing.T) {
	for _, provider := range []string{"cursor", "cursoragent", "Cursor-Agent", "qoder", "qodercli", "Qoder CLI", "omp", "ohmypi", "Oh_My-Pi"} {
		t.Run(provider, func(t *testing.T) {
			home := t.TempDir()
			session := "123e4567-e89b-12d3-a456-426614174010"
			path := filepath.Join(home, ".cursor", "projects", "fixture", "agent-transcripts", session, session+".jsonl")
			family := "cursor"
			if strings.Contains(strings.ToLower(provider), "qoder") {
				family = "qoder"
				path = filepath.Join(home, ".qoder", "projects", "fixture", session+".jsonl")
			} else if provider == "omp" || strings.Contains(strings.ToLower(provider), "my") {
				family = "omp"
				path = filepath.Join(home, ".omp", "agent", "sessions", "fixture", "fixture_"+session+".jsonl")
			}
			record := func(id, text string) map[string]any {
				message := map[string]any{"role": "assistant", "content": []map[string]any{{"type": "text", "text": text}}}
				if family == "cursor" {
					return map[string]any{"role": "assistant", "message": message}
				}
				if family == "omp" {
					return map[string]any{"type": "message", "id": id, "message": message}
				}
				return map[string]any{"type": "assistant", "uuid": id, "message": message}
			}
			seed := record("a1", "seed")
			seed["message"].(map[string]any)["content"] = []map[string]any{{"type": "text", "text": "seed"}, {"type": "tool_use", "id": "tool-1", "name": "Read", "input": map[string]any{"path": "fixture.txt"}}}
			writeInvariantRows(t, path, seed)
			server := testServerWithCacheDir(t.TempDir())
			server.conversationB.Close()
			reader := conversation.NewReader(home)
			browser, err := conversation.NewBrowser(reader, t.TempDir(), conversation.DefaultBrowserOptions())
			if err != nil {
				t.Fatal(err)
			}
			defer browser.Close()
			defer server.herdrC.Close()
			server.conversationM, server.conversationB = reader, browser
			server.state.CommitInventory([]*coordinator.AgentState{{PaneID: "fixture-pane", Agent: provider, Cwd: "/fixture", SessionID: session, AgentSessionID: session, TerminalID: "fixture-terminal", ServerSessionID: "primary", Status: "working"}}, server.state.RevisionCounter())
			agent, _ := server.state.Agent("fixture-pane")
			target := protocol.TargetRef{PaneID: agent.PaneID, TerminalID: agent.TerminalID, Generation: agent.Generation, ServerSessionID: agent.ServerSessionID, AgentSessionID: session}
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
			httpServer := httptest.NewServer(http.HandlerFunc(server.hub.HandleWebSocket))
			defer httpServer.Close()
			defer server.hub.Shutdown(context.Background())
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(httpServer.URL, "http"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			request := func(message map[string]any, result any) bool {
				t.Helper()
				message["pane_id"], message["target"], message["request_id"] = agent.PaneID, target, "fixture-request"
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
				if err := json.Unmarshal(raw, &response); err != nil {
					t.Fatal(err)
				}
				if response.OK && result != nil {
					if err := json.Unmarshal(response.Data, result); err != nil {
						t.Fatal(err)
					}
				}
				return response.OK
			}
			var page conversation.BrowsePage
			if !request(map[string]any{"type": "get_conversation_history", "observe": true}, &page) || page.SubscriptionID == "" || page.Capabilities == nil || page.Capabilities.Source != "recorded_history" || page.Capabilities.ActiveBranch {
				t.Fatalf("observed provider did not attach recorded feed: %+v", page)
			}
			result := map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "tool-1", "content": "recorded tool output"}}}
			toolRecord := map[string]any{"role": "user", "message": result}
			if family == "qoder" {
				toolRecord = map[string]any{"type": "user", "uuid": "tool-result", "message": result}
			} else if family == "omp" {
				toolRecord = map[string]any{"type": "message", "id": "tool-result", "message": map[string]any{"role": "toolResult", "toolCallId": "tool-1", "toolName": "Read", "content": []map[string]any{{"type": "text", "text": "recorded tool output"}}}}
			}
			appendFeedRecord(t, path, toolRecord)
			var changed conversationBatch
			if !request(map[string]any{"type": "get_conversation_updates", "subscription_id": page.SubscriptionID, "source_revision": page.SourceRevision, "after": page.Sequence}, &changed) || changed.Kind != "batch" || len(changed.Entries) != 1 || changed.Entries[0].ID != page.Entries[0].ID || len(changed.Entries[0].Tools) != 1 || changed.Entries[0].Tools[0].Output != "recorded tool output" {
				t.Fatalf("changed existing tool record: %+v", changed)
			}
			page.Sequence = changed.Sequence
			appendFeedRecord(t, path, record("a2", "appended"))
			expected, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var batch conversationBatch
			if !request(map[string]any{"type": "get_conversation_updates", "subscription_id": page.SubscriptionID, "source_revision": page.SourceRevision, "after": page.Sequence}, &batch) || batch.Kind != "batch" || len(batch.Entries) != 1 || batch.Entries[0].Text != "appended" {
				t.Fatalf("changed-record delivery: %+v", batch)
			}
			actual, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(actual, expected) {
				t.Fatal("feed changed transcript bytes")
			}
			target.Generation++
			if request(map[string]any{"type": "get_conversation_updates", "subscription_id": page.SubscriptionID, "source_revision": page.SourceRevision, "after": batch.Sequence}, nil) {
				t.Fatal("updates accepted a replacement target")
			}
		})
	}
}
