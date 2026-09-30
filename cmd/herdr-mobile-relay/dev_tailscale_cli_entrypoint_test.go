//go:build herdr_tailscale_test

package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

const (
	commandFixtureMarker  = "HERDR_SYNTHETIC_TAILSCALE_CLI_FIXTURE_V1"
	commandFixtureLong    = "1.102.4-t3caf7d9e7d-g084ee3b64537"
	commandFixtureStatus  = `{"Version":"` + commandFixtureLong + `","BackendState":"Running","Self":{"ID":"node-fixture","UserID":123,"DNSName":"herdr.tailnet.ts.net."},"CurrentTailnet":{"Name":"fixture-account","MagicDNSSuffix":"tailnet.ts.net","MagicDNSEnabled":true},"CertDomains":["herdr.tailnet.ts.net"],"User":{"123":{"ID":123,"LoginName":"fixture@example.invalid","DisplayName":"Fixture","ProfilePicURL":""}}}`
	commandFixtureVersion = `{"majorMinorPatch":"1.102.4","short":"1.102.4","long":"` + commandFixtureLong + `","gitCommit":"3caf7d9e7dcaba589cfc58beda596929733e4fea","daemonLong":"` + commandFixtureLong + `","extraGitCommit":"084ee3b64537a1276e56fc38cdf0a711da9f4936","osVariant":"appstore","cap":142}`
)

type developmentCommandFixture struct {
	base, home, root, state, coordination string
	relayEnv, config, cache, data         string
	pairingSocket, herdrSocket, herdrBin  string
	cli, cliLog                           string
}

// Keep this public-dispatch inventory aligned with
// docs/tailscale-cli-contract.md#source-entrypoints-and-current-gate. The
// cases invoke run(), the same command dispatcher used by main(), and the
// standalone table covers the public tailscale-cli read paths.
func TestDevelopmentTailscaleCLICommandEntrypointInventory(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("the Go-owned development command requires native Darwin/arm64")
	}

	tests := []struct {
		name        string
		command     string
		action      string
		input       string
		wantCode    int
		wantMessage string
		configure   func(*testing.T, *developmentCommandFixture)
	}{
		{
			name:        "crafted root without validated release layout",
			command:     "dev-tailscale-cli",
			action:      "preflight",
			wantCode:    78,
			wantMessage: "Tailscale CLI permission denied",
			configure:   configureCraftedDevelopmentRoot,
		},
		{
			name:        "wrong HTTPS port",
			command:     "dev-tailscale-cli",
			action:      "preflight",
			wantCode:    78,
			wantMessage: "development ports or side-effect settings differ from the fixed profile",
			configure: func(t *testing.T, _ *developmentCommandFixture) {
				t.Setenv("HERDR_TAILSCALE_CLI_HTTPS_PORT", "9443")
			},
		},
		{
			name:        "wrong backend port",
			command:     "dev-tailscale-cli",
			action:      "preflight",
			wantCode:    78,
			wantMessage: "development ports or side-effect settings differ from the fixed profile",
			configure: func(t *testing.T, _ *developmentCommandFixture) {
				t.Setenv("HERDR_RELAY_PORT", "18379")
			},
		},
		{
			name:        "wrong plugin port",
			command:     "dev-tailscale-cli",
			action:      "preflight",
			wantCode:    78,
			wantMessage: "development ports or side-effect settings differ from the fixed profile",
			configure: func(t *testing.T, _ *developmentCommandFixture) {
				t.Setenv("HERDR_RELAY_PLUGIN_PORT", "18379")
			},
		},
		{
			name:        "production config-root overlap",
			command:     "dev-tailscale-cli",
			action:      "preflight",
			wantCode:    78,
			wantMessage: "Tailscale CLI permission denied",
			configure:   configureProductionConfigOverlap,
		},
		{
			name:        "production state-root overlap",
			command:     "dev-tailscale-cli",
			action:      "preflight",
			wantCode:    78,
			wantMessage: "Tailscale CLI permission denied",
			configure:   configureProductionStateOverlap,
		},
		{
			name:        "installed service-root overlap",
			command:     "dev-tailscale-cli",
			action:      "preflight",
			wantCode:    78,
			wantMessage: "Tailscale CLI permission denied",
			configure:   configureInstalledServiceOverlap,
		},
		{
			name:        "missing development opt-in",
			command:     "dev-tailscale-cli",
			action:      "preflight",
			wantCode:    78,
			wantMessage: "explicit development opt-in and development transport are required",
			configure: func(t *testing.T, _ *developmentCommandFixture) {
				t.Setenv("HERDR_DEV_TAILSCALE_CLI_ENABLE", "")
			},
		},
		{
			name:        "missing route-bound setup consent",
			command:     "dev-tailscale-cli",
			action:      "setup",
			wantCode:    2,
			wantMessage: "route-bound confirmation line is missing or oversized",
		},
		{
			name:        "mismatched route-bound setup consent",
			command:     "dev-tailscale-cli",
			action:      "setup",
			input:       "PUBLISH DEVELOPMENT ROUTE node=other-node origin=https://other.invalid:8443 https-port=8443 backend=127.0.0.1:18377\n",
			wantCode:    2,
			wantMessage: "route-bound confirmation did not exactly match the selected node, listener and backend",
		},
		{
			name:        "standalone preflight refuses",
			command:     "tailscale-cli",
			action:      "preflight",
			wantCode:    2,
			wantMessage: tailscalecli.ErrWorkflowRequired.Error(),
		},
		{
			name:        "standalone status refuses",
			command:     "tailscale-cli",
			action:      "status",
			wantCode:    2,
			wantMessage: tailscalecli.ErrWorkflowRequired.Error(),
		},
		{
			name:        "standalone recover refuses",
			command:     "tailscale-cli",
			action:      "recover",
			wantCode:    2,
			wantMessage: tailscalecli.ErrWorkflowRequired.Error(),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newDevelopmentCommandFixture(t)
			if test.configure != nil {
				test.configure(t, fixture)
			}
			args := []string{test.command, test.action}
			if test.command == "tailscale-cli" {
				args = append(args, "--binary", fixture.cli, "--scope", "development")
			}
			code, stdout, stderr, err := dispatchMainCommand(t, args, test.input)
			if code != test.wantCode || err == nil {
				t.Fatalf("dispatch result = (%d, %v), want refusal code %d", code, err, test.wantCode)
			}
			if !strings.Contains(stderr, test.wantMessage) {
				t.Fatalf("sanitized refusal %q does not contain %q (stdout %q)", stderr, test.wantMessage, stdout)
			}
			combinedOutput := stdout + stderr
			for _, sensitive := range []string{fixture.base, fixture.home, fixture.root, fixture.cli, fixture.cliLog, "fixture@example.invalid"} {
				if strings.Contains(combinedOutput, sensitive) {
					t.Fatalf("refusal leaked a private fixture value %q: %s", sensitive, combinedOutput)
				}
			}
			if _, statErr := os.Lstat(fixture.cliLog); !os.IsNotExist(statErr) {
				t.Fatalf("refused command executed the sentinel synthetic CLI: %v", statErr)
			}
		})
	}

	t.Run("marked synthetic fixture reaches public development workflow", func(t *testing.T) {
		fixture := newDevelopmentCommandFixture(t)
		code, stdout, stderr, err := dispatchMainCommand(t, []string{"dev-tailscale-cli", "preflight"}, "")
		if code != 0 || err != nil {
			t.Fatalf("development preflight = (%d, %v), stderr %q", code, err, stderr)
		}
		for _, expected := range []string{`"node_id":"node-fixture"`, `"origin":"https://herdr.tailnet.ts.net:8443"`, `"profile":"macos-appstore-1.102.4-supplied-metadata"`} {
			if !strings.Contains(stdout, expected) {
				t.Fatalf("synthetic workflow report %q lacks %q", stdout, expected)
			}
		}
		calls, readErr := os.ReadFile(fixture.cliLog)
		if readErr != nil {
			t.Fatalf("positive workflow did not reach its marked synthetic CLI: %v", readErr)
		}
		if got, want := string(calls), "status --json\nversion --json --daemon\nserve status --json\n"; got != want {
			t.Fatalf("synthetic CLI calls = %q, want %q", got, want)
		}
	})

	t.Run("occupied plugin port prevents Serve mutation", func(t *testing.T) {
		fixture := newDevelopmentCommandFixture(t)
		blocker, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: tailscalecli.DevelopmentPluginPort})
		if err != nil {
			t.Fatalf("occupy development plugin port: %v", err)
		}
		defer blocker.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		workflow, cfg, err := developmentCLIFromEnvironment(ctx, "setup")
		if err != nil {
			t.Fatalf("construct isolated fixture workflow: %v", err)
		}
		confirmation := tailscalecli.PublishRouteConfirmation("node-fixture", "https://herdr.tailnet.ts.net:8443",
			tailscalecli.DevelopmentHTTPSPort, tailscalecli.DevelopmentBackendPort)
		code, err := runDevelopmentForeground(ctx, "setup", workflow, cfg, confirmation, io.Discard, io.Discard)
		if code == 0 || err == nil || !strings.Contains(err.Error(), "managed UDP event listener unavailable on 127.0.0.1:18378") {
			t.Fatalf("foreground setup with occupied plugin port = (%d, %v), want startup refusal before Serve publication", code, err)
		}
		calls, err := os.ReadFile(fixture.cliLog)
		if err != nil {
			t.Fatalf("read synthetic CLI invocation sentinel: %v", err)
		}
		for _, call := range strings.FieldsFunc(string(calls), func(r rune) bool { return r == '\n' }) {
			switch call {
			case "status --json", "version --json --daemon", "serve status --json":
			default:
				t.Fatalf("occupied plugin port allowed a non-read-only Tailscale CLI call %q", call)
			}
		}
	})

	t.Run("matching foreign backend health cannot mask bind conflict", func(t *testing.T) {
		fixture := newDevelopmentCommandFixture(t)
		backend, err := net.Listen("tcp", "127.0.0.1:18377")
		if err != nil {
			t.Fatalf("occupy development backend port: %v", err)
		}
		var healthRequests atomic.Int32
		foreign := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			healthRequests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"ok","readiness":"ready","instance":"fixture-instance","tailscale_cli_origin":"https://herdr.tailnet.ts.net:8443"}`)
		})}
		serveDone := make(chan error, 1)
		go func() { serveDone <- foreign.Serve(backend) }()
		defer func() {
			_ = foreign.Close()
			<-serveDone
		}()

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		workflow, cfg, err := developmentCLIFromEnvironment(ctx, "setup")
		if err != nil {
			t.Fatalf("construct isolated fixture workflow: %v", err)
		}
		confirmation := tailscalecli.PublishRouteConfirmation("node-fixture", "https://herdr.tailnet.ts.net:8443",
			tailscalecli.DevelopmentHTTPSPort, tailscalecli.DevelopmentBackendPort)
		code, err := runDevelopmentForeground(ctx, "setup", workflow, cfg, confirmation, io.Discard, io.Discard)
		if code == 0 || err == nil || !strings.Contains(err.Error(), "listen 127.0.0.1:18377") {
			t.Fatalf("foreground setup with occupied backend port = (%d, %v), want owned-listener bind refusal", code, err)
		}
		if got := healthRequests.Load(); got != 0 {
			t.Fatalf("backend readiness contacted a foreign matching responder %d times", got)
		}
		calls, err := os.ReadFile(fixture.cliLog)
		if err != nil {
			t.Fatalf("read synthetic CLI invocation sentinel: %v", err)
		}
		for _, call := range strings.FieldsFunc(string(calls), func(r rune) bool { return r == '\n' }) {
			switch call {
			case "status --json", "version --json --daemon", "serve status --json":
			default:
				t.Fatalf("occupied backend port allowed a non-read-only Tailscale CLI call %q", call)
			}
		}
		pluginProbe, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: tailscalecli.DevelopmentPluginPort})
		if err != nil {
			t.Fatalf("failed startup retained its plugin UDP listener: %v", err)
		}
		_ = pluginProbe.Close()
	})
}

func configureCraftedDevelopmentRoot(t *testing.T, fixture *developmentCommandFixture) {
	t.Helper()
	root := filepath.Join(fixture.base, "crafted-root")
	state := filepath.Join(root, "registration")
	for _, path := range []string{root, state, filepath.Join(root, "config"), filepath.Join(root, "cache"), filepath.Join(root, "data"), filepath.Join(root, "releases")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	marker := "HERDR_DEV_TAILSCALE_CLI_ROOT=1\n" +
		"HERDR_DEV_TAILSCALE_CLI_STATE_ROOT=" + state + "\n" +
		"HERDR_DEV_TAILSCALE_CLI_COORDINATION_ROOT=" + fixture.coordination + "\n"
	writeCommandFixtureFile(t, filepath.Join(root, ".herdr-dev-tailscale-cli"), marker, 0o600)
	pairingSocket, err := tailscalecli.DevelopmentPairingSocketPath(root)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{
		"HERDR_TAILSCALE_CLI_DEVELOPMENT_ROOT": root,
		"HERDR_TAILSCALE_CLI_STATE_ROOT":       state,
		"HERDR_RELAY_ENV":                      filepath.Join(root, "relay.env"),
		"XDG_CONFIG_HOME":                      filepath.Join(root, "config"),
		"XDG_CACHE_HOME":                       filepath.Join(root, "cache"),
		"XDG_DATA_HOME":                        filepath.Join(root, "data"),
		"HERDR_RELEASE_ROOT":                   filepath.Join(root, "data", "herdr-mobile-relay"),
		"HERDR_WEB_ROOT":                       filepath.Join(root, "current", "web"),
		"HERDR_RELAY_BIN":                      filepath.Join(root, "current", "bin", "herdr-mobile-relay"),
		"HERDR_RELAY_PAIRING_SOCKET":           pairingSocket,
	} {
		t.Setenv(key, value)
	}
}

func configureProductionConfigOverlap(t *testing.T, fixture *developmentCommandFixture) {
	t.Helper()
	productionEnv := filepath.Join(fixture.home, "installed", "relay.env")
	writeCommandFixtureFile(t, productionEnv, "XDG_CONFIG_HOME='"+fixture.config+"'\n", 0o600)
	t.Setenv("HERDR_DEV_TAILSCALE_CLI_PRODUCTION_ENV_FILE", productionEnv)
}

func configureProductionStateOverlap(t *testing.T, fixture *developmentCommandFixture) {
	t.Helper()
	productionEnv := filepath.Join(fixture.home, "installed", "relay.env")
	writeCommandFixtureFile(t, productionEnv, "HERDR_TAILSCALE_CLI_STATE_ROOT='"+fixture.root+"'\n", 0o600)
	t.Setenv("HERDR_DEV_TAILSCALE_CLI_PRODUCTION_ENV_FILE", productionEnv)
}

func configureInstalledServiceOverlap(t *testing.T, fixture *developmentCommandFixture) {
	t.Helper()
	service := filepath.Join(fixture.home, "Library", "LaunchAgents", "com.herdr-mobile-relay.service.plist")
	contents := "<?xml version=\"1.0\" encoding=\"UTF-8\"?><plist version=\"1.0\"><dict><key>EnvironmentVariables</key><dict><key>HERDR_RELAY_ENV</key><string>" + fixture.relayEnv + "</string></dict></dict></plist>"
	writeCommandFixtureFile(t, service, contents, 0o600)
}

func newDevelopmentCommandFixture(t *testing.T) *developmentCommandFixture {
	t.Helper()
	base, err := os.MkdirTemp("", "dcli")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	base, err = filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &developmentCommandFixture{
		base: base, home: filepath.Join(base, "home"), root: filepath.Join(base, "development"),
		coordination: filepath.Join(base, "home", ".local", "state", "herdr-mobile-relay", "tailscale-cli-coordination"),
	}
	fixture.state = filepath.Join(fixture.root, "registration")
	fixture.config = filepath.Join(fixture.root, "config")
	fixture.cache = filepath.Join(fixture.root, "cache")
	fixture.data = filepath.Join(fixture.root, "data")
	fixture.relayEnv = filepath.Join(fixture.root, "relay.env")
	fixture.pairingSocket, err = tailscalecli.DevelopmentPairingSocketPath(fixture.root)
	if err != nil {
		t.Fatal(err)
	}
	fixture.herdrSocket = filepath.Join(fixture.home, "herdr", "herdr.sock")
	fixture.herdrBin = filepath.Join(fixture.home, "bin", "herdr")
	fixture.cli = filepath.Join(fixture.home, "bin", "tailscale")
	fixture.cliLog = filepath.Join(base, "cli-invocations.log")
	releaseDir := filepath.Join(fixture.root, "releases", "fixture")
	relayBin := filepath.Join(releaseDir, "bin", "herdr-mobile-relay")
	webDir := filepath.Join(releaseDir, "web")
	for _, path := range []string{
		fixture.home, fixture.root, fixture.state, fixture.config, fixture.cache, fixture.data,
		fixture.coordination, filepath.Join(fixture.root, "releases"), releaseDir,
		filepath.Join(releaseDir, "bin"), webDir, filepath.Dir(fixture.herdrSocket), filepath.Dir(fixture.herdrBin),
	} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join("releases", "fixture"), filepath.Join(fixture.root, "current")); err != nil {
		t.Fatal(err)
	}
	writeCommandFixtureFile(t, filepath.Join(webDir, "version.json"), "{}\n", 0o600)
	writeCommandFixtureFile(t, relayBin, "#!/bin/sh\nexit 0\n", 0o700)
	writeCommandFixtureFile(t, fixture.herdrBin, "#!/bin/sh\nexit 0\n", 0o700)
	cliScript := "#!/bin/sh\n# " + commandFixtureMarker + "\nprintf '%s\\n' \"$*\" >> " + shellQuoteCommandFixture(fixture.cliLog) + "\n" +
		"case \"$*\" in\n" +
		"  'status --json') printf '%s\\n' '" + commandFixtureStatus + "' ;;\n" +
		"  'version --json --daemon') printf '%s\\n' '" + commandFixtureVersion + "' ;;\n" +
		"  'serve status --json') printf '%s\\n' '{}' ;;\n" +
		"  *) exit 91 ;;\n" +
		"esac\n"
	writeCommandFixtureFile(t, fixture.cli, cliScript, 0o700)
	marker := "HERDR_DEV_TAILSCALE_CLI_ROOT=1\n" +
		"HERDR_DEV_TAILSCALE_CLI_STATE_ROOT=" + fixture.state + "\n" +
		"HERDR_DEV_TAILSCALE_CLI_COORDINATION_ROOT=" + fixture.coordination + "\n"
	writeCommandFixtureFile(t, filepath.Join(fixture.root, ".herdr-dev-tailscale-cli"), marker, 0o600)
	values := map[string]string{
		"HERDR_RELAY_TRANSPORT":                 "tailscale-cli",
		"HERDR_RELAY_TOKEN":                     "0123456789abcdef0123456789abcdef",
		"HERDR_RELAY_INSTANCE_ID":               "fixture-instance",
		"HERDR_RELAY_CONTROL_RUN_ID":            "fixture-control-run",
		"HERDR_RELAY_HOST":                      "127.0.0.1",
		"HERDR_RELAY_PORT":                      "18377",
		"HERDR_RELAY_PLUGIN_PORT":               "18378",
		"HERDR_RELAY_PAIRING_SOCKET":            fixture.pairingSocket,
		"HERDR_TAILSCALE_CLI_ORIGIN":            "https://herdr.tailnet.ts.net:8443",
		"HERDR_TAILSCALE_CLI_SCOPE":             "development",
		"HERDR_TAILSCALE_CLI_BIN":               fixture.cli,
		"HERDR_TAILSCALE_CLI_STATE_ROOT":        fixture.state,
		"HERDR_TAILSCALE_CLI_COORDINATION_ROOT": fixture.coordination,
		"HERDR_TAILSCALE_CLI_DEVELOPMENT_ROOT":  fixture.root,
		"HERDR_TAILSCALE_CLI_HTTPS_PORT":        "8443",
		"HERDR_TAILSCALE_CLI_NODE_ID":           "node-fixture",
		"HERDR_PHONE_APP_URL":                   "https://app.example.test",
		"HERDR_BIN":                             fixture.herdrBin,
		"HERDR_SOCKET_PATH":                     fixture.herdrSocket,
		"HERDR_REACHABILITY_PORT_MAPPING":       "0",
		"HERDR_RELAY_REARM_BOOTSTRAP":           "0",
	}
	var relayEnv strings.Builder
	for key, value := range values {
		fmt.Fprintf(&relayEnv, "%s='%s'\n", key, value)
	}
	writeCommandFixtureFile(t, fixture.relayEnv, relayEnv.String(), 0o600)
	for key, value := range map[string]string{
		"HOME":                                        fixture.home,
		"HERDR_DEV_TAILSCALE_CLI_ENABLE":              "1",
		"HERDR_TAILSCALE_CLI_SCOPE":                   "development",
		"HERDR_RELAY_TRANSPORT":                       "tailscale-cli",
		"HERDR_TAILSCALE_CLI_DEVELOPMENT_ROOT":        fixture.root,
		"HERDR_TAILSCALE_CLI_STATE_ROOT":              fixture.state,
		"HERDR_TAILSCALE_CLI_COORDINATION_ROOT":       fixture.coordination,
		"HERDR_RELAY_ENV":                             fixture.relayEnv,
		"XDG_CONFIG_HOME":                             fixture.config,
		"XDG_CACHE_HOME":                              fixture.cache,
		"XDG_DATA_HOME":                               fixture.data,
		"HERDR_RELEASE_ROOT":                          filepath.Join(fixture.data, "herdr-mobile-relay"),
		"HERDR_WEB_ROOT":                              filepath.Join(fixture.root, "current", "web"),
		"HERDR_RELAY_BIN":                             filepath.Join(fixture.root, "current", "bin", "herdr-mobile-relay"),
		"HERDR_RELAY_PAIRING_SOCKET":                  fixture.pairingSocket,
		"HERDR_RELAY_HOST":                            "127.0.0.1",
		"HERDR_RELAY_PORT":                            "18377",
		"HERDR_RELAY_PLUGIN_PORT":                     "18378",
		"HERDR_TAILSCALE_CLI_HTTPS_PORT":              "8443",
		"HERDR_REACHABILITY_PORT_MAPPING":             "0",
		"HERDR_RELAY_REARM_BOOTSTRAP":                 "0",
		"HERDR_RELAY_TOKEN":                           values["HERDR_RELAY_TOKEN"],
		"HERDR_RELAY_INSTANCE_ID":                     values["HERDR_RELAY_INSTANCE_ID"],
		"HERDR_RELAY_CONTROL_RUN_ID":                  values["HERDR_RELAY_CONTROL_RUN_ID"],
		"HERDR_TAILSCALE_CLI_ORIGIN":                  values["HERDR_TAILSCALE_CLI_ORIGIN"],
		"HERDR_TAILSCALE_CLI_BIN":                     fixture.cli,
		"HERDR_TAILSCALE_CLI_NODE_ID":                 values["HERDR_TAILSCALE_CLI_NODE_ID"],
		"HERDR_PHONE_APP_URL":                         values["HERDR_PHONE_APP_URL"],
		"HERDR_BIN":                                   fixture.herdrBin,
		"HERDR_SOCKET_PATH":                           fixture.herdrSocket,
		"HERDR_DEV_TAILSCALE_CLI_PRODUCTION_ENV_FILE": filepath.Join(fixture.home, "installed", "relay.env"),
	} {
		t.Setenv(key, value)
	}
	for _, key := range []string{
		"HERDR_DEV_TAILSCALE_CLI_PORT", "HERDR_DEV_TAILSCALE_CLI_PLUGIN_PORT", "HERDR_DEV_TAILSCALE_CLI_HTTPS_PORT",
		"HERDR_PLUGIN_CONFIG_DIR", "HERDR_TAILSCALE_ORIGIN", "HERDR_EXTERNAL_HTTPS_ORIGIN",
		"HERDR_GATEWAY_URL", "HERDR_GATEWAY_SELECTION", "HERDR_RELAY_RUN_ID",
	} {
		t.Setenv(key, "")
	}
	listener, err := net.Listen("unix", fixture.herdrSocket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return fixture
}

func shellQuoteCommandFixture(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func writeCommandFixtureFile(t *testing.T, path, contents string, mode os.FileMode) {
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

func dispatchMainCommand(t *testing.T, args []string, input string) (int, string, string, error) {
	t.Helper()
	stdin, err := os.CreateTemp(t.TempDir(), "stdin-")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.WriteString(input); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	stdout, err := os.CreateTemp(t.TempDir(), "stdout-")
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.CreateTemp(t.TempDir(), "stderr-")
	if err != nil {
		t.Fatal(err)
	}
	oldStdin, oldStdout, oldStderr := os.Stdin, os.Stdout, os.Stderr
	os.Stdin, os.Stdout, os.Stderr = stdin, stdout, stderr
	defer func() { os.Stdin, os.Stdout, os.Stderr = oldStdin, oldStdout, oldStderr }()
	code, dispatchErr := run(args)
	if dispatchErr != nil {
		reportError(os.Stderr, args, dispatchErr)
	}
	readFile := func(file *os.File) string {
		if _, seekErr := file.Seek(0, io.SeekStart); seekErr != nil {
			t.Fatal(seekErr)
		}
		contents, readErr := io.ReadAll(file)
		if readErr != nil {
			t.Fatal(readErr)
		}
		return string(contents)
	}
	return code, readFile(stdout), readFile(stderr), dispatchErr
}
