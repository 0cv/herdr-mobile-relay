package tailscalecli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestDevelopmentIsolationRequiresGoOwnedOptInAndLayout(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("the real development profile is Darwin/arm64 only")
	}
	layout, home, root, coordination := developmentIsolationFixture(t)
	if err := layout.validate(false, false); err != nil {
		t.Fatalf("valid isolated layout refused: %v", err)
	}

	t.Run("opt-in is required", func(t *testing.T) {
		t.Setenv("HERDR_DEV_TAILSCALE_CLI_ENABLE", "")
		if err := layout.validate(false, false); !errors.Is(err, ErrWorkflowRequired) {
			t.Fatalf("missing opt-in = %v", err)
		}
	})

	t.Run("production scope is refused", func(t *testing.T) {
		t.Setenv("HERDR_TAILSCALE_CLI_SCOPE", "production")
		if err := layout.validate(false, false); !errors.Is(err, ErrWorkflowRequired) {
			t.Fatalf("production scope = %v", err)
		}
	})

	t.Run("caller-selected XDG root is refused", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
		if err := layout.validate(false, false); !errors.Is(err, ErrPermissionDenied) {
			t.Fatalf("non-isolated XDG config root = %v", err)
		}
	})

	t.Run("installed production state overlap is refused", func(t *testing.T) {
		productionEnv := filepath.Join(home, "installed", "relay.env")
		if err := os.MkdirAll(filepath.Dir(productionEnv), 0o700); err != nil {
			t.Fatal(err)
		}
		contents := "XDG_CONFIG_HOME='" + filepath.Join(root, "config") + "'\n"
		writeDevelopmentFixtureFile(t, productionEnv, contents, 0o600)
		t.Setenv("HERDR_DEV_TAILSCALE_CLI_PRODUCTION_ENV_FILE", productionEnv)
		if err := layout.validate(false, false); !errors.Is(err, ErrPermissionDenied) {
			t.Fatalf("development/production overlap = %v", err)
		}
	})

	t.Run("installed service environment is discovered read-only", func(t *testing.T) {
		productionEnv := filepath.Join(home, "installed", "relay.env")
		if err := os.MkdirAll(filepath.Dir(productionEnv), 0o700); err != nil {
			t.Fatal(err)
		}
		writeDevelopmentFixtureFile(t, productionEnv,
			"HERDR_TAILSCALE_CLI_COORDINATION_ROOT='"+coordination+"'\n", 0o600)
		servicePath := filepath.Join(home, ".config", "systemd", "user", "herdr-mobile-relay.service")
		if runtime.GOOS == "darwin" {
			servicePath = filepath.Join(home, "Library", "LaunchAgents", "com.herdr-mobile-relay.service.plist")
			if err := os.MkdirAll(filepath.Dir(servicePath), 0o700); err != nil {
				t.Fatal(err)
			}
			writeDevelopmentFixtureFile(t, servicePath,
				"<?xml version=\"1.0\" encoding=\"UTF-8\"?><plist version=\"1.0\"><dict><key>EnvironmentVariables</key><dict><key>HERDR_RELAY_ENV</key><string>"+productionEnv+"</string></dict></dict></plist>", 0o600)
		} else {
			if err := os.MkdirAll(filepath.Dir(servicePath), 0o700); err != nil {
				t.Fatal(err)
			}
			writeDevelopmentFixtureFile(t, servicePath, "Environment=HERDR_RELAY_ENV="+productionEnv+"\n", 0o600)
		}
		t.Setenv("HERDR_DEV_TAILSCALE_CLI_PRODUCTION_ENV_FILE", productionEnv)
		if err := layout.validate(false, false); err != nil {
			t.Fatalf("coexisting installed service with separate roots refused: %v", err)
		}
	})

	t.Run("cleanup requires a stopped relay", func(t *testing.T) {
		listener, err := net.Listen("unix", layout.pairingSocket)
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		if err := layout.validate(true, false); err == nil || !strings.Contains(err.Error(), "stop the foreground development relay") {
			t.Fatalf("live pairing socket was not refused for cleanup: %v", err)
		}
	})

	t.Run("setup validates the host Herdr socket", func(t *testing.T) {
		listener, err := net.Listen("unix", layout.herdrSocket)
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		if err := layout.validate(false, true); err != nil {
			t.Fatalf("private setup with an owner Herdr socket refused: %v", err)
		}
	})
}

func TestDevelopmentManagerEnforcesActionLocalGuards(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("the real development profile is Darwin/arm64 only")
	}
	layout, _, _, _ := developmentIsolationFixture(t)
	calls := 0
	client := newTestClientForPlatform(os.Getenv("HERDR_TAILSCALE_CLI_BIN"), func(context.Context, string, ...string) (commandResult, error) {
		calls++
		return commandResult{}, nil
	}, "darwin", "arm64")
	client.validateBinary = true
	manager, err := newDevelopmentManager(layout.root, layout.state, layout.coordination, client)
	if err != nil {
		t.Fatal(err)
	}
	manager.developmentIsolation = layout

	pairingListener, err := net.Listen("unix", layout.pairingSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer pairingListener.Close()
	if err := manager.ReserveBackendPort(context.Background(), "fixture-instance", "development", "node-fixture",
		"https://relay.tailnet.ts.net:8443", DevelopmentHTTPSPort, DevelopmentBackendPort,
		"00000000000000000000000000000001"); err == nil || !strings.Contains(err.Error(), "stop the foreground development relay") {
		t.Fatalf("reservation was not refused while the relay was running: %v", err)
	}

	request := fixtureRequest(true)
	if err := manager.Publish(context.Background(), request); err == nil || !strings.Contains(err.Error(), "local Herdr Unix socket is unavailable") {
		t.Fatalf("publication was not refused without the host Herdr socket: %v", err)
	}
	if calls != 0 {
		t.Fatalf("action-local refusal executed the CLI %d times", calls)
	}
}

func TestDevelopmentIsolationEnvironmentParsersFailClosed(t *testing.T) {
	for _, input := range []string{
		"Environment=OTHER=/tmp/relay.env\n",
		"Environment=HERDR_RELAY_ENV=\n",
		"Environment=HERDR_RELAY_ENV=/tmp/one\nEnvironment=HERDR_RELAY_ENV=/tmp/two\n",
		"Environment=HERDR_RELAY_ENV=relative\n",
	} {
		if got, err := parseSystemdRelayEnvironment([]byte(input)); err == nil || got != "" {
			t.Errorf("unsafe systemd environment %q parsed as %q, err=%v", input, got, err)
		}
	}
	if got, err := parseSystemdRelayEnvironment([]byte("Environment=HERDR_RELAY_ENV=\"/tmp/relay env\"\n")); err != nil || got != "/tmp/relay env" {
		t.Fatalf("quoted systemd environment = %q, %v", got, err)
	}
	plist := []byte("<?xml version=\"1.0\"?><plist version=\"1.0\"><dict><key>EnvironmentVariables</key><dict><key>HERDR_RELAY_ENV</key><string>/tmp/relay.env</string></dict></dict></plist>")
	if got, err := parseLaunchdRelayEnvironment(plist); err != nil || got != "/tmp/relay.env" {
		t.Fatalf("launchd environment = %q, %v", got, err)
	}
	for _, input := range []string{
		"<plist><dict><key>EnvironmentVariables</key><dict><key>HERDR_RELAY_ENV</key><string>relative</string></dict></dict></plist>",
		"<plist><dict><key>Other</key><string>/tmp/relay.env</string></dict></plist>",
		"<plist><dict><key>EnvironmentVariables</key><dict><key>HERDR_RELAY_ENV</key><string>/tmp/one</string><key>HERDR_RELAY_ENV</key><string>/tmp/two</string></dict></dict></plist>",
		"<plist><dict><key>EnvironmentVariables</key><dict><key>HERDR_RELAY_ENV</key><string>/tmp/relay.env</string></dict></dict></plist><plist><dict/></plist>",
	} {
		if got, err := parseLaunchdRelayEnvironment([]byte(input)); err == nil || got != "" {
			t.Errorf("unsafe launchd environment %q parsed as %q, err=%v", input, got, err)
		}
	}
}

func developmentIsolationFixture(t *testing.T) (*developmentIsolation, string, string, string) {
	t.Helper()
	tempBase, err := os.MkdirTemp("", "di")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tempBase) })
	base, err := filepath.EvalSymlinks(tempBase)
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(base, "h")
	root := filepath.Join(base, "d")
	coordination := filepath.Join(home, ".local", "state", "herdr-mobile-relay", "tailscale-cli-coordination")
	for _, path := range []string{
		home, root, filepath.Join(root, "registration"), filepath.Join(root, "config"),
		filepath.Join(root, "cache"), filepath.Join(root, "data"), filepath.Join(root, "releases"),
		filepath.Join(root, "releases", "fixture", "bin"), filepath.Join(root, "releases", "fixture", "web"),
		coordination,
	} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("releases/fixture", filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	writeDevelopmentFixtureFile(t, filepath.Join(root, "releases", "fixture", "web", "version.json"), "{}\n", 0o600)
	relayBinary := filepath.Join(root, "releases", "fixture", "bin", "herdr-mobile-relay")
	writeDevelopmentFixtureFile(t, relayBinary, "#!/bin/sh\nexit 0\n", 0o700)
	configCLI := filepath.Join(home, "bin", "tailscale")
	herdrBinary := filepath.Join(home, "bin", "herdr")
	for _, binary := range []string{configCLI, herdrBinary} {
		if err := os.MkdirAll(filepath.Dir(binary), 0o700); err != nil {
			t.Fatal(err)
		}
		writeDevelopmentFixtureFile(t, binary, "#!/bin/sh\nexit 0\n", 0o700)
	}
	herdrSocket := filepath.Join(home, "herdr", "herdr.sock")
	if err := os.MkdirAll(filepath.Dir(herdrSocket), 0o700); err != nil {
		t.Fatal(err)
	}
	pairingSocket := filepath.Join(root, "config", "pairing-control.sock")
	state := filepath.Join(root, "registration")
	relayEnv := filepath.Join(root, "relay.env")
	values := map[string]string{
		"HERDR_RELAY_TRANSPORT":                 "tailscale-cli",
		"HERDR_RELAY_TOKEN":                     "0123456789abcdef0123456789abcdef",
		"HERDR_RELAY_INSTANCE_ID":               "fixture-instance",
		"HERDR_RELAY_CONTROL_RUN_ID":            "fixture-control-run",
		"HERDR_RELAY_HOST":                      "127.0.0.1",
		"HERDR_RELAY_PORT":                      strconv.Itoa(DevelopmentBackendPort),
		"HERDR_RELAY_PLUGIN_PORT":               strconv.Itoa(DevelopmentPluginPort),
		"HERDR_RELAY_PAIRING_SOCKET":            pairingSocket,
		"HERDR_TAILSCALE_CLI_ORIGIN":            "https://relay.tailnet.ts.net:8443",
		"HERDR_TAILSCALE_CLI_SCOPE":             "development",
		"HERDR_TAILSCALE_CLI_BIN":               configCLI,
		"HERDR_TAILSCALE_CLI_STATE_ROOT":        state,
		"HERDR_TAILSCALE_CLI_COORDINATION_ROOT": coordination,
		"HERDR_TAILSCALE_CLI_DEVELOPMENT_ROOT":  root,
		"HERDR_TAILSCALE_CLI_HTTPS_PORT":        strconv.Itoa(DevelopmentHTTPSPort),
		"HERDR_TAILSCALE_CLI_NODE_ID":           "node-fixture",
		"HERDR_PHONE_APP_URL":                   "https://app.example.test",
		"HERDR_BIN":                             herdrBinary,
		"HERDR_SOCKET_PATH":                     herdrSocket,
		"HERDR_REACHABILITY_PORT_MAPPING":       "0",
		"HERDR_RELAY_REARM_BOOTSTRAP":           "0",
	}
	var env strings.Builder
	for key, value := range values {
		fmt.Fprintf(&env, "%s='%s'\n", key, value)
	}
	writeDevelopmentFixtureFile(t, relayEnv, env.String(), 0o600)
	marker := "HERDR_DEV_TAILSCALE_CLI_ROOT=1\n" +
		"HERDR_DEV_TAILSCALE_CLI_STATE_ROOT=" + state + "\n" +
		"HERDR_DEV_TAILSCALE_CLI_COORDINATION_ROOT=" + coordination + "\n"
	writeDevelopmentFixtureFile(t, filepath.Join(root, ".herdr-dev-tailscale-cli"), marker, 0o600)

	for key, value := range map[string]string{
		"HOME":                                        home,
		"HERDR_DEV_TAILSCALE_CLI_ENABLE":              "1",
		"HERDR_TAILSCALE_CLI_SCOPE":                   "development",
		"HERDR_RELAY_TRANSPORT":                       "tailscale-cli",
		"HERDR_TAILSCALE_CLI_DEVELOPMENT_ROOT":        root,
		"HERDR_TAILSCALE_CLI_STATE_ROOT":              state,
		"HERDR_TAILSCALE_CLI_COORDINATION_ROOT":       coordination,
		"HERDR_RELAY_ENV":                             relayEnv,
		"XDG_CONFIG_HOME":                             filepath.Join(root, "config"),
		"XDG_CACHE_HOME":                              filepath.Join(root, "cache"),
		"XDG_DATA_HOME":                               filepath.Join(root, "data"),
		"HERDR_RELEASE_ROOT":                          filepath.Join(root, "data", "herdr-mobile-relay"),
		"HERDR_WEB_ROOT":                              filepath.Join(root, "current", "web"),
		"HERDR_RELAY_BIN":                             filepath.Join(root, "current", "bin", "herdr-mobile-relay"),
		"HERDR_RELAY_PAIRING_SOCKET":                  pairingSocket,
		"HERDR_RELAY_HOST":                            "127.0.0.1",
		"HERDR_RELAY_PORT":                            strconv.Itoa(DevelopmentBackendPort),
		"HERDR_RELAY_PLUGIN_PORT":                     strconv.Itoa(DevelopmentPluginPort),
		"HERDR_TAILSCALE_CLI_HTTPS_PORT":              strconv.Itoa(DevelopmentHTTPSPort),
		"HERDR_REACHABILITY_PORT_MAPPING":             "0",
		"HERDR_RELAY_REARM_BOOTSTRAP":                 "0",
		"HERDR_RELAY_TOKEN":                           values["HERDR_RELAY_TOKEN"],
		"HERDR_RELAY_INSTANCE_ID":                     values["HERDR_RELAY_INSTANCE_ID"],
		"HERDR_RELAY_CONTROL_RUN_ID":                  values["HERDR_RELAY_CONTROL_RUN_ID"],
		"HERDR_TAILSCALE_CLI_ORIGIN":                  values["HERDR_TAILSCALE_CLI_ORIGIN"],
		"HERDR_TAILSCALE_CLI_BIN":                     values["HERDR_TAILSCALE_CLI_BIN"],
		"HERDR_TAILSCALE_CLI_NODE_ID":                 values["HERDR_TAILSCALE_CLI_NODE_ID"],
		"HERDR_PHONE_APP_URL":                         values["HERDR_PHONE_APP_URL"],
		"HERDR_BIN":                                   herdrBinary,
		"HERDR_SOCKET_PATH":                           herdrSocket,
		"HERDR_DEV_TAILSCALE_CLI_PRODUCTION_ENV_FILE": filepath.Join(home, "installed", "relay.env"),
	} {
		t.Setenv(key, value)
	}
	for _, name := range []string{
		"HERDR_DEV_TAILSCALE_CLI_PORT", "HERDR_DEV_TAILSCALE_CLI_PLUGIN_PORT", "HERDR_DEV_TAILSCALE_CLI_HTTPS_PORT",
		"HERDR_PLUGIN_CONFIG_DIR", "HERDR_TAILSCALE_ORIGIN", "HERDR_EXTERNAL_HTTPS_ORIGIN",
		"HERDR_GATEWAY_URL", "HERDR_GATEWAY_SELECTION", "HERDR_RELAY_RUN_ID",
	} {
		t.Setenv(name, "")
	}
	return &developmentIsolation{
		home: home, root: root, state: state, coordination: coordination,
		relayEnv: relayEnv, configHome: filepath.Join(root, "config"), cacheHome: filepath.Join(root, "cache"),
		dataHome: filepath.Join(root, "data"), releaseRoot: filepath.Join(root, "data", "herdr-mobile-relay"),
		runtimeRoot: filepath.Join(root, "runtime"), webRoot: filepath.Join(root, "current", "web"),
		relayBinary: filepath.Join(root, "current", "bin", "herdr-mobile-relay"), pairingSocket: pairingSocket,
		herdrBinary: herdrBinary, herdrSocket: herdrSocket,
	}, home, root, coordination
}

func writeDevelopmentFixtureFile(t *testing.T, path, contents string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}
