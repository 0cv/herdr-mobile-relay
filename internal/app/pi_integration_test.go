package app

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

func TestPiIntegrationAuthorization(t *testing.T) {
	for _, operation := range []string{"get_pi_integration", "set_pi_integration"} {
		metadata, known := protocol.ClassifyAction(operation)
		if !known || operation == "set_pi_integration" && (!metadata.Audited || !metadata.RequiresProtocol) {
			t.Fatal("missing privileged metadata")
		}
		for _, role := range []string{"reader", "bootstrap", "controller", ""} {
			for _, authenticated := range []bool{false, true} {
				allowed := authenticated && (operation == "get_pi_integration" || role == "controller")
				denied := authorizeAuthenticatedIdentity(transport.AuthenticatedIdentity{Role: role}, authenticated, metadata, "") != nil
				if denied == allowed {
					t.Fatalf("%s %s %v", operation, role, authenticated)
				}
			}
		}
	}
}

func TestPiIntegrationAudit(t *testing.T) {
	e := newPiFrameEnv(t)
	e.piIntegrationFrame(t, map[string]any{"type": "set_pi_integration", "enabled": true, "instance": "private", "path": "not allowed"})

	details := e.auditAttempt(t, "set_pi_integration").Details
	if len(details) != 1 || details["enabled"] != true {
		t.Fatalf("unsafe audit fields: %#v", details)
	}
}

func TestPiIntegrationRelayStartRefresh(t *testing.T) {
	home, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv(agentroots.PiListEnv, "")
	s := New(&config.Config{ReleaseRoot: t.TempDir()}, "test", "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if s.conversationB != nil {
		defer s.conversationB.Close()
	}
	s.refreshPiIntegration()
	if s.piIntegration.Status().Enabled {
		t.Fatal("startup installed without consent")
	}
	state := s.piIntegration.Set(true)
	if !state.Enabled {
		t.Fatalf("%+v", state)
	}
	path := filepath.Join(home, ".pi/agent/extensions/herdr-mobile-relay-commands/index.ts")
	if err := os.WriteFile(path, []byte("outdated"), 0600); err != nil {
		t.Fatal(err)
	}
	s.refreshPiIntegration()
	if s.piIntegration.Status().Profiles[0].Status != "up to date" {
		t.Fatal("startup failed to refresh")
	}
	for _, name := range []string{"index.ts", "bridge.mjs", "activity.mjs"} {
		installed, err := os.ReadFile(filepath.Join(home, ".pi/agent/extensions/herdr-mobile-relay-commands", name))
		if err != nil {
			t.Fatal(err)
		}
		source, err := os.ReadFile(filepath.Join("../../relay/pi-command-bridge", name))
		if err != nil {
			t.Fatal(err)
		}
		if string(installed) != string(source) {
			t.Fatal("installed integration differs from relay/pi-command-bridge", name)
		}
	}
}
