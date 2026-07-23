package blackbox

// Regression tests for transport/lifecycle contracts:
//   - /readyz must be 503 until Herdr inventory has succeeded (§13.1)
//   - /healthz must expose the served web/ bundle version+hash (§13.1)
//   - WebSocket connections must close gracefully on shutdown, not be dropped
//     (finding #3: srv.Shutdown does not drain hijacked conns)
// Reuses freePort/repoRoot/waitForHealth/setupEnv from relay_test.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// startRelayBrokenHerdr launches a relay whose herdr binary always fails, so
// inventory never becomes ready. /health (static liveness) still returns 200.
func startRelayBrokenHerdr(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()

	relayBin := filepath.Join(tmpDir, "relay")
	build := exec.Command("go", "build", "-o", relayBin, "./cmd/herdr-mobile-relay")
	build.Dir = repoRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build relay: %v\n%s", err, out)
	}

	webDir := filepath.Join(tmpDir, "web")
	os.MkdirAll(webDir, 0o755)
	os.WriteFile(filepath.Join(webDir, "index.html"), []byte("<html></html>"), 0o644)

	port := freePort(t)
	pluginPort := freePort(t)

	cmd := exec.Command(relayBin)
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("HERDR_RELAY_PORT=%d", port),
		fmt.Sprintf("HERDR_RELAY_PLUGIN_PORT=%d", pluginPort),
		"HERDR_RELAY_HOST=127.0.0.1",
		"HERDR_RELAY_TOKEN=",
		"HERDR_RELAY_INSTANCE_ID=broken-test",
		"HERDR_BIN=/bin/false", // every herdr call exits 1 → inventory never ready
		fmt.Sprintf("HERDR_WEB_ROOT=%s", webDir),
		"HERDR_RELAY_POLL_INTERVAL=0.5",
		fmt.Sprintf("XDG_CONFIG_HOME=%s", filepath.Join(tmpDir, "config")),
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start relay: %v", err)
	}
	t.Cleanup(func() {
		cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			cmd.Process.Kill()
		}
	})

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitForHealth(t, base)
	return base
}

// §13.1: /readyz must return 200 only after a successful Herdr inventory.
func TestReadyzNotReadyBeforeInventory(t *testing.T) {
	base := startRelayBrokenHerdr(t)

	// Give the poller a few intervals; inventory keeps failing.
	time.Sleep(1500 * time.Millisecond)

	resp, err := http.Get(base + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("/readyz = %d with no successful inventory, want 503 "+
			"(§13.1: 200 only after Herdr readiness; readiness is gated on listening, not inventory)",
			resp.StatusCode)
	}
}

// §13.1: /healthz must expose the served web/ bundle version and hash so an
// interrupted upgrade pairing a new binary with an old bundle is detectable.
func TestHealthzIncludesBundleIdentity(t *testing.T) {
	env := setupEnv(t)

	resp, err := http.Get(env.httpBase + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)

	found := false
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
		if strings.Contains(strings.ToLower(k), "bundle") {
			found = true
		}
	}
	if !found {
		t.Fatalf("/healthz exposes no served-bundle version/hash field (keys: %v) "+
			"(§13.1: expose bundle version+hash to detect a new-binary/old-bundle upgrade)", keys)
	}
}

// finding #3: on shutdown the relay must close WebSocket connections gracefully
// (a going-away/normal close frame), not drop them when the process exits.
func TestWebSocketClosesGracefullyOnShutdown(t *testing.T) {
	env := setupEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, env.wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusInternalError, "")

	// Let the handshake settle, then ask the relay to shut down.
	time.Sleep(500 * time.Millisecond)
	env.relayCmd.Process.Signal(os.Interrupt)

	// Read until the connection ends; capture the terminal error.
	var closeErr error
	for {
		readCtx, rc := context.WithTimeout(ctx, 5*time.Second)
		_, _, err := conn.Read(readCtx)
		rc()
		if err != nil {
			closeErr = err
			break
		}
	}

	status := websocket.CloseStatus(closeErr)
	if status != websocket.StatusGoingAway && status != websocket.StatusNormalClosure {
		t.Fatalf("WebSocket ended with close status %v on shutdown, want going-away(1001)/normal(1000) "+
			"(finding #3: srv.Shutdown does not drain hijacked WebSockets; no registry+WaitGroup+CloseNow)", status)
	}
}
