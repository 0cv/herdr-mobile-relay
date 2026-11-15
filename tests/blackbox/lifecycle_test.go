package blackbox

// Regression tests for transport/lifecycle contracts:
//   - /readyz must be 503 until Herdr inventory has succeeded (§13.1)
//   - /healthz must expose the served web/ bundle version+hash (§13.1)
//   - WebSocket connections must close gracefully on shutdown, not be dropped
//     (finding #3: srv.Shutdown does not drain hijacked conns)
// Reuses freePort/repoRoot/waitForStatus/setupEnv from relay_test.go.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// startRelayBrokenHerdr launches a relay whose herdr binary always fails, so
// inventory never becomes ready. /health (static liveness) still returns 200.
func startRelayBrokenHerdr(t *testing.T) string {
	t.Helper()
	return startRelay(t, relayOptions{instanceID: "broken-test", brokenHerdr: true}).httpBase
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

	ctx, conn := dial(t, env, 15*time.Second)

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
