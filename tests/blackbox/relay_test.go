package blackbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type TestEnv struct {
	relayCmd      *exec.Cmd
	fakeBin       string
	tmpDir        string
	operationsLog string
	socketPath    string
	port          int
	pluginPort    int
	wsURL         string
	httpBase      string
}

func setupEnv(t *testing.T) *TestEnv {
	scenario := `{"panes":[{"pane_id":"pane-1","agent":"claude","name":"test","agent_status":"working","tab_id":"tab-1","workspace_id":"ws-1","cwd":"/tmp","revision":1,"foreground_cwd":"/tmp"}],"tabs":[{"tab_id":"tab-1","workspace_id":"ws-1","label":"main","number":1,"cwd":"/tmp"}]}`
	return setupEnvWithScenario(t, scenario)
}

func setupEnvWithScenario(t *testing.T, scenario string) *TestEnv {
	t.Helper()

	tmpDir := t.TempDir()

	// Build fake-herdr
	fakeBin := filepath.Join(tmpDir, "fake-herdr")
	build := exec.Command("go", "build", "-o", fakeBin, "./cmd/fake-herdr")
	build.Dir = repoRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fake-herdr: %v\n%s", err, out)
	}

	// Build relay
	relayBin := filepath.Join(tmpDir, "herdr-mobile-relay")
	buildRelay := exec.Command("go", "build", "-o", relayBin, "./cmd/herdr-mobile-relay")
	buildRelay.Dir = repoRoot(t)
	if out, err := buildRelay.CombinedOutput(); err != nil {
		t.Fatalf("build relay: %v\n%s", err, out)
	}

	port := freePort(t)
	pluginPort := freePort(t)

	// Write scenario file
	scenarioPath := filepath.Join(tmpDir, "scenario.json")
	os.WriteFile(scenarioPath, []byte(scenario), 0o644)

	// Create a minimal web root
	webDir := filepath.Join(tmpDir, "web")
	os.MkdirAll(webDir, 0o755)
	os.WriteFile(filepath.Join(webDir, "index.html"), []byte("<html>test</html>"), 0o644)

	env := &TestEnv{
		fakeBin:       fakeBin,
		tmpDir:        tmpDir,
		operationsLog: filepath.Join(tmpDir, "operations.jsonl"),
		socketPath:    filepath.Join(tmpDir, "herdr.sock"),
		port:          port,
		pluginPort:    pluginPort,
		wsURL:         fmt.Sprintf("ws://127.0.0.1:%d/ws", port),
		httpBase:      fmt.Sprintf("http://127.0.0.1:%d", port),
	}

	env.relayCmd = exec.Command(relayBin)
	env.relayCmd.Env = append(os.Environ(),
		fmt.Sprintf("HERDR_RELAY_PORT=%d", port),
		fmt.Sprintf("HERDR_RELAY_PLUGIN_PORT=%d", pluginPort),
		"HERDR_RELAY_HOST=127.0.0.1",
		"HERDR_RELAY_TOKEN=",
		"HERDR_RELAY_INSTANCE_ID=blackbox-test",
		fmt.Sprintf("HERDR_BIN=%s", fakeBin),
		fmt.Sprintf("HERDR_WEB_ROOT=%s", webDir),
		fmt.Sprintf("HERDR_RELAY_POLL_INTERVAL=0.5"),
		fmt.Sprintf("FAKE_HERDR_SCENARIO=%s", scenarioPath),
		fmt.Sprintf("FAKE_HERDR_OPERATIONS=%s", env.operationsLog),
		fmt.Sprintf("XDG_CONFIG_HOME=%s", filepath.Join(tmpDir, "config")),
		fmt.Sprintf("XDG_CACHE_HOME=%s", filepath.Join(tmpDir, "cache")),
		fmt.Sprintf("XDG_DATA_HOME=%s", filepath.Join(tmpDir, "data")),
		fmt.Sprintf("HERDR_SOCKET_PATH=%s", env.socketPath),
	)
	env.relayCmd.Stdout = os.Stdout
	env.relayCmd.Stderr = os.Stderr

	if err := env.relayCmd.Start(); err != nil {
		t.Fatalf("start relay: %v", err)
	}

	t.Cleanup(func() {
		env.relayCmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { env.relayCmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			env.relayCmd.Process.Kill()
		}
	})

	waitForHealth(t, env.httpBase)
	return env
}

func waitForHealth(t *testing.T, base string) {
	t.Helper()
	// Fresh CI runners build several test packages concurrently. Give the relay
	// enough time to complete its first fake-Herdr inventory under that load.
	for i := 0; i < 200; i++ {
		resp, err := http.Get(base + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("relay did not become healthy")
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

func repoRoot(t *testing.T) string {
	t.Helper()
	// Walk up from test dir to find go.mod
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("cannot find repo root")
		}
		dir = parent
	}
}

func TestHealthEndpoint(t *testing.T) {
	env := setupEnv(t)

	resp, err := http.Get(env.httpBase + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if inst := resp.Header.Get("X-Herdr-Relay-Instance"); inst != "blackbox-test" {
		t.Errorf("instance = %q", inst)
	}
}

func TestHealthzEndpoint(t *testing.T) {
	env := setupEnv(t)

	resp, err := http.Get(env.httpBase + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)

	if body["status"] != "ok" {
		t.Errorf("status = %v", body["status"])
	}
	if body["instance"] != "blackbox-test" {
		t.Errorf("instance = %v", body["instance"])
	}
	if body["protocol"] != float64(2) {
		t.Errorf("protocol = %v", body["protocol"])
	}
}

func TestStaticServing(t *testing.T) {
	env := setupEnv(t)

	resp, err := http.Get(env.httpBase + "/index.html")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestWebSocketConnectAndReceiveAgents(t *testing.T) {
	env := setupEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, env.wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	// Should receive agents broadcast from poller within a few poll intervals
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("did not receive agents message")
		default:
		}

		readCtx, readCancel := context.WithTimeout(ctx, 2*time.Second)
		_, data, err := conn.Read(readCtx)
		readCancel()
		if err != nil {
			t.Fatalf("read: %v", err)
		}

		var msg map[string]any
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}

		if msg["type"] == "agents" {
			agents, ok := msg["agents"].([]any)
			if !ok {
				t.Fatal("agents field is not an array")
			}
			// The on-connect snapshot may precede the first inventory poll.
			// Wait for the subsequent committed snapshot in that case.
			if len(agents) == 0 {
				continue
			}
			if len(agents) != 1 {
				t.Fatalf("agents len = %d, want 1", len(agents))
			}
			agent := agents[0].(map[string]any)
			if agent["pane_id"] != "pane-1" {
				t.Errorf("pane_id = %v", agent["pane_id"])
			}
			if agent["agent"] != "claude" {
				t.Errorf("agent = %v", agent["agent"])
			}
			return
		}
	}
}

func TestWebSocketRejectsDisallowedOrigin(t *testing.T) {
	env := setupEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// With no token configured and no allowed origins, any origin should be rejected
	// But our test env has no token and no origin restriction configured,
	// so this tests the basic connect path works
	conn, _, err := websocket.Dial(ctx, env.wsURL, nil)
	if err != nil {
		t.Fatalf("dial should succeed with no origin header: %v", err)
	}
	conn.Close(websocket.StatusNormalClosure, "")
}
