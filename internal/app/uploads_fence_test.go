package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
	"github.com/0cv/herdr-mobile-relay/internal/upload"
	"github.com/coder/websocket"
)

func TestUploadHandlersCompleteABatchWithoutAManagedFence(t *testing.T) {
	server := testServer()
	manager, err := upload.NewManager(upload.Config{Root: t.TempDir(), ChunkBytes: 1024, MaxFiles: 2, MaxFileBytes: 4096, MaxBatchBytes: 8192})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	server.uploadM = manager
	server.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane", TerminalID: "terminal", SessionID: "agent-session", Agent: "copilot", Status: "idle",
	}}, server.state.RevisionCounter())
	agent, ok := server.state.Agent("pane")
	if !ok {
		t.Fatal("upload target agent was not committed")
	}
	connection := openUploadHandlerClient(t, server, nil)
	target := map[string]any{
		"server_session_id": "primary", "pane_id": agent.PaneID, "terminal_id": agent.TerminalID,
		"generation": agent.Generation, "agent_session_id": agent.SessionID,
	}
	begin := sendUploadHandlerRequest(t, connection, map[string]any{
		"type": "upload_begin", "request_id": "begin", "target": target,
		"files": []map[string]any{{"name": "note.txt", "media_type": "text/plain", "bytes": 5}},
	})
	beginResult, ok := begin["result"].(map[string]any)
	if !ok || beginResult["upload_id"] == "" {
		t.Fatalf("begin result = %#v", begin)
	}
	uploadID := beginResult["upload_id"].(string)
	data := []byte("hello")
	digest := sha256.Sum256(data)
	digestText := hex.EncodeToString(digest[:])
	chunk := sendUploadHandlerRequest(t, connection, map[string]any{
		"type": "upload_chunk", "request_id": "chunk", "target": target, "upload_id": uploadID,
		"file_index": 0, "sequence": 0, "data": data, "sha256": digestText,
	})
	if chunk["result"] == nil {
		t.Fatalf("chunk result = %#v", chunk)
	}
	finish := sendUploadHandlerRequest(t, connection, map[string]any{
		"type": "upload_finish", "request_id": "finish", "target": target, "upload_id": uploadID,
		"files": []map[string]any{{"file_index": 0, "sha256": digestText}},
	})
	finishResult, ok := finish["result"].(map[string]any)
	if !ok || len(finishResult["attachments"].([]any)) != 1 {
		t.Fatalf("finish result = %#v", finish)
	}
}

func TestUploadHandlersRejectEachMutationWhenTheManagedFenceIsBlocked(t *testing.T) {
	server := testServer()
	fence := newManagedCommandFence(func() *coordinator.CommandResult {
		return &coordinator.CommandResult{Action: "upload", Phase: "not_started", Error: "Managed runtime changed before execution"}
	})
	connection := openUploadHandlerClient(t, server, fence)
	for _, action := range []string{"upload_begin", "upload_chunk", "upload_finish"} {
		result := sendUploadHandlerRequest(t, connection, map[string]any{"type": action, "request_id": action})
		if result["phase"] != "not_started" {
			t.Fatalf("blocked %s result = %#v", action, result)
		}
	}
}

func openUploadHandlerClient(t *testing.T, server *Server, fence *managedCommandFence) *websocket.Conn {
	t.Helper()
	server.hub.SetHandler(func(client *transport.ClientConn, message map[string]any, admitted func()) {
		defer admitted()
		requestID, _ := message["request_id"].(string)
		switch message["type"] {
		case "upload_begin":
			server.handleUploadBegin(client, requestID, message, fence)
		case "upload_chunk":
			server.handleUploadChunk(client, requestID, message, fence)
		case "upload_finish":
			server.handleUploadFinish(client, requestID, message, fence)
		}
	})
	httpServer := httptest.NewServer(http.HandlerFunc(server.hub.HandleWebSocket))
	connection, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(httpServer.URL, "http"), nil)
	if err != nil {
		httpServer.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = connection.CloseNow()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.hub.Shutdown(ctx)
		httpServer.Close()
	})
	return connection
}

func sendUploadHandlerRequest(t *testing.T, connection *websocket.Conn, message map[string]any) map[string]any {
	t.Helper()
	payload, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := connection.Write(ctx, websocket.MessageText, payload); err != nil {
		t.Fatal(err)
	}
	_, payload, err = connection.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(payload, &result); err != nil {
		t.Fatal(err)
	}
	return result
}
