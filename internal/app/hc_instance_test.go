package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/hc"
	"github.com/0cv/herdr-mobile-relay/internal/piintegration"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
	"github.com/coder/websocket"
)

func hcTestServer(t *testing.T) *Server {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv(agentroots.PiListEnv, "")
	s := New(&config.Config{
		Backend: "hc", Host: "127.0.0.1", SocketPath: filepath.Join(root, "core.sock"),
		ConfigHome: filepath.Join(root, "config"), CacheDir: filepath.Join(root, "cache"),
		RuntimeDir: filepath.Join(root, "runtime"), ReleaseRoot: filepath.Join(root, "release"),
	}, "test", "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() {
		s.herdrC.Close()
		if s.conversationB != nil {
			s.conversationB.Close()
		}
	})
	return s
}

func TestHCInstanceDefaults(t *testing.T) {
	s := hcTestServer(t)
	if _, ok := s.herdrC.(*hc.Client); !ok {
		t.Fatalf("backend: %T", s.herdrC)
	}
	if s.paneSizeM.Available() {
		t.Fatal("core must not lease pane sizes")
	}
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "relay"
	} else if idx := strings.Index(hostname, "."); idx > 0 {
		hostname = hostname[:idx]
	}
	if s.hostname != hostname+" (core)" {
		t.Fatal(s.hostname)
	}
	for _, cap := range s.effectiveCapabilitiesFor(s.herdrC.CapabilityStatus()) {
		if slices.Contains([]string{"self_update", "app_deploy", "pi_integration"}, cap) {
			t.Fatal(cap)
		}
	}
	if !slices.Contains(protocol.Capabilities, "self_update") {
		t.Fatal("modified shared capabilities")
	}
}

func TestHCStartupLeavesPiIntegrationUntouched(t *testing.T) {
	s := hcTestServer(t)
	dir := filepath.Join(s.home, ".pi", "agent")
	if state := s.piIntegration.Set(true); !state.Enabled {
		t.Fatalf("%+v", state)
	}
	path := filepath.Join(dir, "extensions", "herdr-mobile-relay-commands", "index.ts")
	if err := os.WriteFile(path, []byte("owned original"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s.refreshPiIntegration()
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatalf("core changed Pi extension: %v", err)
	}
	calls := 0
	s.piIntegration = piintegration.New(nil, func() []string { calls++; return []string{dir} })
	s.refreshPiIntegration()
	if calls != 0 {
		t.Fatal("core invoked Pi refresh")
	}
}

func TestHCRefusesOwnerCommandsWithoutInvokingManagers(t *testing.T) {
	s := hcTestServer(t)
	s.updateM = nil
	s.appDeployM = nil
	s.piIntegration = piintegration.New(nil, func() []string { panic("Pi integration invoked") })
	s.hub.SetHandler(func(client *transport.ClientConn, msg map[string]any, admitted func()) {
		defer admitted()
		inbound, err := protocol.DecodeMap(msg)
		if err != nil {
			t.Error(err)
			return
		}
		if inbound.Type == "set_pi_integration" {
			s.handlePiIntegration(context.Background(), client, inbound, msg)
		} else {
			s.handleUpdate(context.Background(), client, inbound)
		}
	})
	server := httptest.NewServer(http.HandlerFunc(s.hub.HandleWebSocket))
	defer server.Close()
	defer s.hub.Shutdown(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	for _, action := range []string{"set_pi_integration", "check_update", "install_update", "deploy_app_update"} {
		request, _ := json.Marshal(map[string]any{"type": action, "request_id": action, "protocol": protocol.Version, "enabled": true, "instance": "core"})
		if err := conn.Write(ctx, websocket.MessageText, request); err != nil {
			t.Fatal(err)
		}
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var result map[string]any
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatal(err)
		}
		if result["type"] != "command_result" || result["action"] != action || result["ok"] != false || result["phase"] != "failed" || !strings.Contains(result["error"].(string), "TUI relay") {
			t.Fatalf("%s: %s", action, data)
		}
	}
}
