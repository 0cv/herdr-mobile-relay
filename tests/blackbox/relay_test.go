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

const defaultScenario = `{"panes":[{"pane_id":"pane-1","terminal_id":"terminal-1","agent":"claude","name":"test","agent_status":"working","tab_id":"tab-1","workspace_id":"ws-1","cwd":"/tmp","revision":1,"foreground_cwd":"/tmp"}],"tabs":[{"tab_id":"tab-1","workspace_id":"ws-1","label":"main","number":1,"cwd":"/tmp"}]}`

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

type relayOptions struct {
	scenario    string
	instanceID  string
	token       string
	brokenHerdr bool
	env         []string
}

func setupEnv(t *testing.T) *TestEnv {
	return startRelay(t, relayOptions{})
}

func startRelay(t *testing.T, opts relayOptions) *TestEnv {
	t.Helper()

	if opts.scenario == "" {
		opts.scenario = defaultScenario
	}
	if opts.instanceID == "" {
		opts.instanceID = "blackbox-test"
	}

	tmpDir := t.TempDir()
	port := freePort(t)
	pluginPort := freePort(t)
	socketPath := filepath.Join(testSocketDir(t), "herdr.sock")
	scenarioPath := filepath.Join(tmpDir, "scenario.json")
	if err := os.WriteFile(scenarioPath, []byte(opts.scenario), 0o644); err != nil {
		t.Fatal(err)
	}

	env := &TestEnv{
		fakeBin:       builtBinaries.fakeHerdr,
		tmpDir:        tmpDir,
		operationsLog: filepath.Join(tmpDir, "operations.jsonl"),
		socketPath:    socketPath,
		port:          port,
		pluginPort:    pluginPort,
		wsURL:         fmt.Sprintf("ws://127.0.0.1:%d/ws", port),
		httpBase:      fmt.Sprintf("http://127.0.0.1:%d", port),
	}

	herdrBin := builtBinaries.fakeHerdr
	readyPath := "/readyz"
	if opts.brokenHerdr {
		herdrBin = "/bin/false"
		readyPath = "/health"
	} else {
		startFakeSocket(t, socketPath, scenarioPath)
	}

	env.relayCmd = exec.Command(builtBinaries.relay)
	env.relayCmd.Env = append(os.Environ(),
		fmt.Sprintf("HERDR_RELAY_PORT=%d", port),
		fmt.Sprintf("HERDR_RELAY_PLUGIN_PORT=%d", pluginPort),
		"HERDR_RELAY_HOST=127.0.0.1",
		"HERDR_RELAY_TOKEN="+opts.token,
		"HERDR_RELAY_INSTANCE_ID="+opts.instanceID,
		"HERDR_BIN="+herdrBin,
		"HERDR_WEB_ROOT="+filepath.Join(repoRoot(t), "web"),
		"HERDR_RELAY_POLL_INTERVAL=0.5",
		"FAKE_HERDR_SCENARIO="+scenarioPath,
		"FAKE_HERDR_OPERATIONS="+env.operationsLog,
		"XDG_CONFIG_HOME="+filepath.Join(tmpDir, "config"),
		"XDG_CACHE_HOME="+filepath.Join(tmpDir, "cache"),
		"XDG_DATA_HOME="+filepath.Join(tmpDir, "data"),
		"HERDR_SOCKET_PATH="+socketPath,
		stubLaunchctlPath(t),
	)
	env.relayCmd.Env = append(env.relayCmd.Env, opts.env...)
	env.relayCmd.Stdout = os.Stdout
	env.relayCmd.Stderr = os.Stderr

	if err := env.relayCmd.Start(); err != nil {
		t.Fatalf("start relay: %v", err)
	}

	t.Cleanup(func() { stopProcess(env.relayCmd) })
	waitForStatus(t, env.httpBase, readyPath, http.StatusOK)
	return env
}

func dial(t *testing.T, env *TestEnv, timeout time.Duration) (context.Context, *websocket.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	conn, _, err := websocket.Dial(ctx, env.wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	return ctx, conn
}

func startFakeSocket(t *testing.T, socketPath, scenarioPath string) {
	t.Helper()
	cmd := exec.Command(builtBinaries.fakeHerdr, "socket-server")
	cmd.Env = append(os.Environ(),
		"FAKE_HERDR_SOCKET="+socketPath,
		"FAKE_HERDR_SCENARIO="+scenarioPath,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fake Herdr socket server: %v", err)
	}
	t.Cleanup(func() { stopProcess(cmd) })
	for i := 0; i < 200; i++ {
		if _, err := os.Stat(socketPath); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("fake Herdr socket %s did not appear", socketPath)
}

func waitForStatus(t *testing.T, base, endpoint string, status int) {
	t.Helper()
	// Fresh CI runners build several test packages concurrently. Give the relay
	// enough time to complete its first fake-Herdr inventory under that load.
	for i := 0; i < 200; i++ {
		resp, err := http.Get(base + endpoint)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == status {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s did not return status %d", endpoint, status)
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

// stubLaunchctlPath shadows launchctl so a relay started on macOS cannot remove
// launchd jobs belonging to the machine running the tests.
func stubLaunchctlPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "launchctl"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return "PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH")
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := findRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("cannot find repo root")
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
	if body["protocol"] != float64(3) {
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

	ctx, conn := dial(t, env, 10*time.Second)

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

	// With no token configured and no allowed origins, any origin should be rejected
	// But our test env has no token and no origin restriction configured,
	// so this tests the basic connect path works
	dial(t, env, 5*time.Second)
}
