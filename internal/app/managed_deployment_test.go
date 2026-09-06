package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/coder/websocket"
)

func TestManagedDeploymentOmitsAndRejectsRemoteUpdates(t *testing.T) {
	root := t.TempDir()
	webRoot := filepath.Join(root, "web")
	if err := os.MkdirAll(webRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(webRoot, "index.html"), []byte("<html></html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	cfg := &config.Config{
		Host: "127.0.0.1", Port: port, InstanceID: "managed-test", ManagedDeployment: true,
		WebRoot: webRoot, RuntimeDir: filepath.Join(root, "runtime"), CacheDir: filepath.Join(root, "cache"),
		ConfigHome: filepath.Join(root, "config"), ReleaseRoot: filepath.Join(root, "release"), HerdrBin: "/bin/false",
	}
	server := New(cfg, "0.20.8", "managed-revision", slog.New(slog.NewTextHandler(io.Discard, nil)))
	runContext, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(runContext) }()
	t.Cleanup(func() {
		stop()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("server shutdown: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("server did not stop")
		}
	})

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(5 * time.Second)
	for {
		response, requestErr := http.Get(base + "/health")
		if requestErr == nil {
			response.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("managed relay did not start: %v", requestErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
	connection, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(base, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.CloseNow() })

	configMessage := readManagedMessage(t, connection, func(message map[string]any) bool { return message["type"] == "push_config" })
	rawCapabilities, _ := configMessage["capabilities"].([]any)
	capabilities := make([]string, 0, len(rawCapabilities))
	for _, capability := range rawCapabilities {
		capabilities = append(capabilities, fmt.Sprint(capability))
	}
	if slices.Contains(capabilities, "self_update") || slices.Contains(capabilities, "app_deploy") {
		t.Fatalf("managed capabilities advertise update/deploy: %v", capabilities)
	}

	for _, action := range []string{"install_update", "deploy_app_update"} {
		requestID := "deny-" + action
		payload, err := json.Marshal(map[string]any{
			"type": action, "request_id": requestID, "protocol": protocol.Version,
			"expected_version": "0.20.9", "expected_revision": "off-manifest",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := connection.Write(context.Background(), websocket.MessageText, payload); err != nil {
			t.Fatal(err)
		}
		message := readManagedMessage(t, connection, func(message map[string]any) bool { return message["request_id"] == requestID })
		apiError, _ := message["error"].(map[string]any)
		if message["type"] != "error" || apiError["code"] != protocol.ErrorManagedDeployment {
			t.Fatalf("%s response = %#v, want stable managed policy error", action, message)
		}
	}
}

func TestDeploymentCapabilities(t *testing.T) {
	base := []string{"self_update", "other"}
	managed := deploymentCapabilities(base, true, true)
	if slices.Contains(managed, "self_update") || slices.Contains(managed, "app_deploy") {
		t.Fatalf("managed capabilities = %v", managed)
	}
	if !slices.Equal(base, []string{"self_update", "other"}) {
		t.Fatalf("input capabilities mutated: %v", base)
	}
	unmanaged := deploymentCapabilities(base, false, true)
	if !slices.Contains(unmanaged, "self_update") || !slices.Contains(unmanaged, "app_deploy") {
		t.Fatalf("unmanaged capabilities = %v", unmanaged)
	}
	plain := deploymentCapabilities(base, false, false)
	if slices.Contains(plain, "app_deploy") {
		t.Fatalf("unconfigured app deploy advertised: %v", plain)
	}
}

func TestManagedReadinessRequiresExactGenerationInventory(t *testing.T) {
	root := t.TempDir()
	expectedPath := filepath.Join(root, "expected.json")
	writeExpected := func(contents string) {
		t.Helper()
		if err := os.WriteFile(expectedPath, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeExpected(`{"version":1,"generation":"g1","panes":[{"pane_id":"pane-1","native_session_id":"session-1","profile_id":"personal"}]}`)
	server := New(&config.Config{
		ManagedDeployment: true, ExpectedInventoryPath: expectedPath, ActiveGeneration: "g1",
		RuntimeDir: filepath.Join(root, "runtime"), CacheDir: filepath.Join(root, "cache"),
	}, "0.20.8", "revision", slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.ready = true
	server.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", SessionID: "session-1", ProfileID: "personal", Status: "idle",
	}}, server.state.RevisionCounter())

	response := httptest.NewRecorder()
	server.handleReadyz(response, httptest.NewRequest("GET", "/readyz", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status":"ready"`) {
		t.Fatalf("exact readiness = %d %s", response.Code, response.Body.String())
	}

	server.state.CommitInventory(nil, server.state.RevisionCounter())
	response = httptest.NewRecorder()
	server.handleReadyz(response, httptest.NewRequest("GET", "/readyz", nil))
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"state":"unexpected_empty"`) {
		t.Fatalf("collapsed readiness = %d %s", response.Code, response.Body.String())
	}
	health := httptest.NewRecorder()
	server.handleHealthz(health, httptest.NewRequest("GET", "/healthz", nil))
	if !strings.Contains(health.Body.String(), `"readiness":"blocked"`) {
		t.Fatalf("health accepted collapsed managed inventory: %s", health.Body.String())
	}

	writeExpected(`{"version":1,"generation":"g1","acknowledged_empty":true,"panes":[]}`)
	response = httptest.NewRecorder()
	server.handleReadyz(response, httptest.NewRequest("GET", "/readyz", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status":"acknowledged_empty"`) {
		t.Fatalf("acknowledged-empty readiness = %d %s", response.Code, response.Body.String())
	}
	health = httptest.NewRecorder()
	server.handleHealthz(health, httptest.NewRequest("GET", "/healthz", nil))
	if !strings.Contains(health.Body.String(), `"readiness":"acknowledged_empty"`) {
		t.Fatalf("health collapsed acknowledged-empty state: %s", health.Body.String())
	}
	server.state.MarkInventoryFailure(fmt.Errorf("fixture inventory failure"))
	health = httptest.NewRecorder()
	server.handleHealthz(health, httptest.NewRequest("GET", "/healthz", nil))
	if !strings.Contains(health.Body.String(), `"readiness":"degraded"`) {
		t.Fatalf("health hid inventory degradation: %s", health.Body.String())
	}
}

func TestServerReconcilesPersistedProfileOwnership(t *testing.T) {
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(filepath.Join(configHome, "herdr"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"personal", "emu"} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(configHome, "herdr", "agent-profiles.ini"), []byte("[config]\nreplace_profiles = true\n[profiles]\npersonal = Personal\nemu = EMU\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	stateDir := filepath.Join(root, "state")
	newServer := func() *Server {
		return New(&config.Config{
			ConfigHome: configHome, RuntimeDir: stateDir, CacheDir: filepath.Join(root, "cache"),
			ReleaseRoot: filepath.Join(root, "release"), HerdrBin: "/bin/false",
		}, "test", "revision", slog.New(slog.NewTextHandler(io.Discard, nil)))
	}
	first := newServer()
	first.profiles.Remember("pane-personal", "personal")
	agents := []*coordinator.AgentState{
		{PaneID: "pane-personal", Agent: "copilot", Session: "session-personal"},
		{PaneID: "pane-unknown", Agent: "copilot"},
	}
	first.reconcileProfileOwnership(agents)
	if agents[0].ProfileID != "personal" || agents[1].ProfileID != "" {
		t.Fatalf("first ownership = %q, %q", agents[0].ProfileID, agents[1].ProfileID)
	}
	restarted := newServer()
	reloaded := []*coordinator.AgentState{{PaneID: "pane-personal", Agent: "copilot", Session: "session-personal"}}
	restarted.reconcileProfileOwnership(reloaded)
	if reloaded[0].ProfileID != "personal" {
		t.Fatalf("reloaded ownership = %q", reloaded[0].ProfileID)
	}
}

func TestServerRecordsProfileAssociationFailure(t *testing.T) {
	root := t.TempDir()
	statePath := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(statePath, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := New(&config.Config{
		RuntimeDir: statePath, CacheDir: filepath.Join(root, "cache"), HerdrBin: "/bin/false",
	}, "test", "revision", slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.reconcileProfileOwnership(nil)
	if len(server.recentSafeErrors()) == 0 {
		t.Fatal("association failure was not recorded")
	}
}

func readManagedMessage(t *testing.T, connection *websocket.Conn, accept func(map[string]any) bool) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		_, data, err := connection.Read(ctx)
		if err != nil {
			t.Fatalf("read managed relay message: %v", err)
		}
		var message map[string]any
		if err := json.Unmarshal(data, &message); err != nil {
			t.Fatal(err)
		}
		if accept(message) {
			return message
		}
	}
}
