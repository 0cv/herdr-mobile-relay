package app

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/deviceauth"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
	"github.com/0cv/herdr-mobile-relay/internal/web"
)

func TestNativePiActivityBrowserFixture(t *testing.T) {
	work := os.Getenv("PI_NATIVE_BROWSER_WORK")
	if work == "" {
		t.Skip("requires an explicitly launched private stock Pi PTY")
	}
	if os.Getenv("HOME") != filepath.Join(work, "home") || os.Getenv("PI_OFFLINE") != "1" {
		t.Fatal("private offline fixture environment required")
	}
	var identity struct {
		PID     int    `json:"pid"`
		Session string `json:"session"`
		Mode    string `json:"mode"`
	}
	raw, err := os.ReadFile(filepath.Join(work, "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(raw, &identity) != nil || identity.PID <= 0 || identity.Session == "" || identity.Mode != "tui" {
		t.Fatal("invalid private stock identity")
	}
	token := (rand.Text() + rand.Text())[:32]
	cfg := &config.Config{Host: "127.0.0.1", Token: token, InstanceID: "private-native-live", RuntimeDir: filepath.Join(work, "relay-runtime"), CacheDir: filepath.Join(work, "relay-cache"), ConfigHome: filepath.Join(work, "relay-config"), WebRoot: os.Getenv("PI_NATIVE_BROWSER_WEB"), SocketPath: filepath.Join(work, "herdr.sock"), HerdrBin: "must-not-execute-owner-herdr"}
	server := New(cfg, "0.22.9", "native-synthetic", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if server.initErr != nil || server.deviceAuth == nil {
		t.Fatalf("fixture initialization: %v", server.initErr)
	}
	server.state.CommitInventory([]*coordinator.AgentState{{PaneID: "fixture-pane", TerminalID: "fixture-terminal", ServerSessionID: "primary", Agent: "pi", Cwd: filepath.Join(work, "project"), SessionID: identity.Session, AgentSessionID: identity.Session, Project: "Native live", Status: "working", ConversationHistoryAvailable: true}}, server.state.RevisionCounter())
	server.webH, err = web.NewHandler(cfg.WebRoot)
	if err != nil {
		t.Fatal(err)
	}
	role := deviceauth.RoleController
	var invitation *deviceauth.Invitation
	if requested := os.Getenv("PI_NATIVE_BROWSER_ROLE"); requested != "" {
		if requested != string(deviceauth.RoleReader) {
			t.Fatal("unsupported private browser role")
		}
		role = deviceauth.RoleReader
		created, err := server.deviceAuth.CreateInvitation("Private reader", role, "en")
		if err != nil {
			t.Fatal(err)
		}
		invitation = &created
	}
	agent, _ := server.state.Agent("fixture-pane")
	var authenticated atomic.Bool
	var mutations atomic.Int64
	var received sync.Map
	server.hub.SetOnConnect(func(client *transport.ClientConn) {
		if id, ok := client.Identity(); ok && id.CredentialID != "" && id.Role == string(role) {
			authenticated.Store(true)
		}
		server.hub.Send(client, map[string]any{"type": "push_config", "protocol": 3, "version": "native-synthetic", "host": "private-native", "capabilities": []string{"conversation_history", "conversation_activity"}, "inventory": map[string]any{"state": "ready", "stale": false}})
		server.hub.Send(client, map[string]any{"type": "agents", "agents": []*coordinator.AgentState{agent}})
	})
	server.hub.SetOnDisconnect(func(client *transport.ClientConn) { server.stopConversationFeed(client.ID(), "") })
	server.hub.SetHandler(func(client *transport.ClientConn, message map[string]any, admitted func()) {
		defer admitted()
		inbound, err := protocol.DecodeMap(message)
		if err != nil {
			return
		}
		counter, _ := received.LoadOrStore(inbound.Type, &atomic.Int64{})
		counter.(*atomic.Int64).Add(1)
		switch inbound.Type {
		case "get_conversation_history", "get_conversation_updates", "unwatch_conversation":
			action, _ := protocol.ClassifyAction(inbound.Type)
			if server.authorizeDeviceAction(client, action, "") != nil || validateExactPaneTarget(server.state, inbound, true) != nil {
				return
			}
			switch inbound.Type {
			case "get_conversation_history":
				server.handleConversationHistory(client, inbound)
			case "get_conversation_updates":
				server.handleConversationUpdates(client, inbound)
			case "unwatch_conversation":
				server.stopConversationFeed(client.ID(), inbound.SubscriptionID)
				server.sendCommandResult(client, inbound.RequestID, inbound.Type, true, "completed", "", inbound.PaneID, nil)
			}
		case "send_input", "send_keys", "lease_pane_size", "submit_prompt":
			mutations.Add(1)
		case "refresh_agents":
			server.hub.Send(client, map[string]any{"type": "agents", "agents": []*coordinator.AgentState{agent}})
		case "push_viewed_pane":
		case "acknowledge_pane":
			if role == deviceauth.RoleReader {
				mutations.Add(1)
			}
		default:
			action, known := protocol.ClassifyAction(inbound.Type)
			if !known || action.Class == protocol.ActionMutating || strings.HasPrefix(inbound.Type, "agent_") {
				mutations.Add(1)
			}
		}
	})
	httpServer := httptest.NewServer(http.HandlerFunc(server.handleRoot))
	defer httpServer.Close()
	defer server.hub.Shutdown(context.Background())
	if server.conversationB != nil {
		defer server.conversationB.Close()
	}
	defer server.herdrC.Close()
	configBytes, _ := json.Marshal(map[string]any{"url": httpServer.URL, "ws": "ws" + strings.TrimPrefix(httpServer.URL, "http"), "token": token, "role": role, "invitation": invitation})
	if err := os.WriteFile(filepath.Join(work, "browser-config.json"), configBytes, 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(work, "browser-done")); err == nil {
			if !authenticated.Load() || mutations.Load() != 0 {
				t.Fatal("paired read-only browser contract failed")
			}
			methods := map[string]int64{}
			received.Range(func(key, value any) bool { methods[key.(string)] = value.(*atomic.Int64).Load(); return true })
			result, _ := json.Marshal(map[string]any{"authenticated_browser": authenticated.Load(), "role": role, "agent_mutations": mutations.Load(), "received_methods": methods})
			if err := os.WriteFile(filepath.Join(work, "browser-result.json"), result, 0600); err != nil {
				t.Fatal(err)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("private enrolled browser did not complete")
}
