package main

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/app"
	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/deviceauth"
	"github.com/0cv/herdr-mobile-relay/internal/localcontrol"
	"github.com/0cv/herdr-mobile-relay/internal/release"
	"github.com/0cv/herdr-mobile-relay/internal/support"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
	"github.com/andybalholm/brotli"
)

func installedCommandEnv(t *testing.T, mode string) string {
	t.Helper()
	root := t.TempDir()
	for _, key := range []string{
		"HERDR_GATEWAY_URL", "HERDR_GATEWAY_SELECTION", "HERDR_RELAY_REARM_BOOTSTRAP",
		"HERDR_RELAY_RUN_ID", "HERDR_RELAY_CONTROL_RUN_ID", "HERDR_RELAY_PAIRING_SOCKET",
		"HERDR_TAILSCALE_ORIGIN", "HERDR_EXTERNAL_HTTPS_ORIGIN", "HERDR_PHONE_APP_URL",
		"HERDR_TAILSCALE_CLI_ORIGIN", "HERDR_TAILSCALE_CLI_SCOPE", "HERDR_TAILSCALE_CLI_STATE_ROOT",
		"HERDR_TAILSCALE_CLI_COORDINATION_ROOT", "HERDR_TAILSCALE_CLI_DEVELOPMENT_ROOT",
	} {
		t.Setenv(key, "")
	}
	for key, value := range map[string]string{
		"HOME": root, "XDG_CONFIG_HOME": filepath.Join(root, "config"),
		"XDG_CACHE_HOME": filepath.Join(root, "cache"), "XDG_DATA_HOME": filepath.Join(root, "data"),
		"HERDR_RELAY_ENV":    filepath.Join(root, "runtime", "relay.env"),
		"HERDR_RELEASE_ROOT": filepath.Join(root, "release"), "HERDR_WEB_ROOT": filepath.Join(root, "web"),
		"HERDR_BIN": filepath.Join(root, "absent-herdr"), "HERDR_SOCKET_PATH": filepath.Join(root, "absent-socket"),
		"HERDR_RELAY_TOKEN": strings.Repeat("k", 32), "HERDR_RELAY_INSTANCE_ID": "installed-command-fixture",
		"HERDR_RELAY_TRANSPORT": mode, "HERDR_RELAY_HOST": "127.0.0.1",
		"HERDR_RELAY_PORT": "8375", "HERDR_RELAY_PLUGIN_PORT": "8376",
		"HERDR_RELAY_LOG_FORMAT": "text", "HERDR_RELAY_LOG_LEVEL": "error",
	} {
		t.Setenv(key, value)
	}
	if mode == config.TransportGateway {
		t.Setenv("HERDR_GATEWAY_URL", "wss://gateway.example.test")
	}
	prior := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prior) })
	return root
}

func TestSupportCommandProjectsLoadedPrivateState(t *testing.T) {
	root := installedCommandEnv(t, config.TransportCloudflare)
	if err := os.MkdirAll(filepath.Join(root, "runtime"), 0o700); err != nil {
		t.Fatal(err)
	}
	dir, err := filepath.EvalSymlinks(filepath.Join(root, "runtime"))
	if err != nil || os.Chmod(filepath.Dir(dir), 0o700) != nil || os.Chmod(dir, 0o700) != nil {
		t.Fatal("private fixture path unavailable")
	}
	const secret = "command-private-canary"
	if err := support.Write(dir, support.Snapshot{
		Version: secret, Revision: secret, Protocol: 3, ReleaseDirectory: secret,
		WebHash: secret, WebVersion: secret, Readiness: "ready",
		Inventory:  map[string]any{"state": "ready", secret: map[string]any{secret: secret}},
		Components: map[string]string{secret: secret}, Scheduler: map[string]any{secret: secret},
		Transport: map[string]any{secret: secret}, UDP: map[string]any{secret: secret},
		RecentErrors: []string{secret}, ActivityFailures: 4, PollFailures: 0,
	}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(support.Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	code, runErr, output := captureRunStdout(t, []string{"support"})
	if runtime.GOOS != "linux" {
		if code != 1 || runErr == nil || len(output) != 0 {
			t.Fatal("unsupported native acquisition returned a document")
		}
	} else {
		if code != 0 || runErr != nil || len(output) > 4096 || bytes.Contains(output, []byte(secret)) ||
			!bytes.Contains(output, []byte(`"protocol": 3`)) ||
			!bytes.Contains(output, []byte(`"activity_failures": 4`)) ||
			bytes.Contains(output, []byte(`"release_directory"`)) {
			t.Fatal("real support command did not emit only bounded historical projection")
		}
	}
	after, err := os.ReadFile(support.Path(dir))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("support command mutated private fixture")
	}
	if err := os.WriteFile(support.Path(dir), []byte(`{"recent_errors":["`+secret+`"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, runErr, output = captureRunStdout(t, []string{"support"})
	if code != 1 || runErr == nil || len(output) != 0 || strings.Contains(runErr.Error(), secret) {
		t.Fatal("failed acquisition emitted private output")
	}
	code, runErr, output = captureRunStdout(t, []string{"support", "extra"})
	if code != 2 || runErr == nil || len(output) != 0 {
		t.Fatal("support usage failure emitted a document")
	}
}

func TestInstalledCommandUsesRealNonArmingConstructor(t *testing.T) {
	for _, mode := range []string{config.TransportGateway, config.TransportCloudflare} {
		t.Run(mode, func(t *testing.T) {
			root := installedCommandEnv(t, mode)
			path := filepath.Join(root, "runtime", "device-auth", "devices.json")
			var before []byte
			for restart := 0; restart < 3; restart++ {
				called := false
				code, err := runWithServerRunner([]string{"serve-installed"}, func(server *app.Server, ctx context.Context) error {
					called = true
					if server == nil || ctx.Err() != nil {
						t.Fatal("installed command did not hand a real server to the lifecycle")
					}
					data, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					var disk struct {
						Invitation  json.RawMessage         `json:"invitation"`
						Credentials []deviceauth.Credential `json:"credentials"`
					}
					if err := json.Unmarshal(data, &disk); err != nil {
						t.Fatal(err)
					}
					if len(disk.Invitation) != 0 || len(disk.Credentials) != 0 {
						t.Fatal("public installed command armed an invitation")
					}
					if before != nil && !bytes.Equal(before, data) {
						t.Fatal("public installed restart changed persisted state")
					}
					before = data
					return nil
				})
				if code != 0 || err != nil || !called {
					t.Fatalf("installed dispatch = (%d, %v), runner called=%v", code, err, called)
				}
			}
		})
	}
}

func TestForegroundCommandCompatibilityStillEnsuresBootstrap(t *testing.T) {
	for _, args := range [][]string{nil, {"serve"}} {
		t.Run(fmtCommandArgs(args), func(t *testing.T) {
			root := installedCommandEnv(t, config.TransportCloudflare)
			called := false
			code, err := runWithServerRunner(args, func(_ *app.Server, _ context.Context) error {
				called = true
				store, err := deviceauth.Open(filepath.Join(root, "runtime", "device-auth"))
				if err != nil || !store.BootstrapStatus().Armed {
					t.Fatal("ordinary/default serve no longer ensures initial bootstrap")
				}
				return nil
			})
			if code != 0 || err != nil || !called {
				t.Fatalf("foreground dispatch = (%d, %v), called=%v", code, err, called)
			}
		})
	}
}

func fmtCommandArgs(args []string) string {
	if len(args) == 0 {
		return "default"
	}
	return strings.Join(args, "-")
}

func TestInstalledCommandRefusesBeforeConstructionOrOwnership(t *testing.T) {
	for _, test := range []struct {
		name     string
		args     []string
		key      string
		value    string
		wantCode int
	}{
		{name: "extra arguments", args: []string{"serve-installed", "extra"}, wantCode: 2},
		{name: "tokenless", key: "HERDR_RELAY_TOKEN", wantCode: 1},
		{name: "rearm", key: "HERDR_RELAY_REARM_BOOTSTRAP", value: "true", wantCode: 1},
		{name: "managed identity", key: "HERDR_RELAY_RUN_ID", value: "foreground", wantCode: 1},
		{name: "control identity", key: "HERDR_RELAY_CONTROL_RUN_ID", value: "foreground", wantCode: 1},
		{name: "control socket", key: "HERDR_RELAY_PAIRING_SOCKET", value: "/fixture/control", wantCode: 1},
		{name: "managed", key: "HERDR_RELAY_TRANSPORT", value: config.TransportTailscale, wantCode: 1},
		{name: "BYO", key: "HERDR_RELAY_TRANSPORT", value: config.TransportTailscaleExternal, wantCode: 1},
		{name: "CLI", key: "HERDR_RELAY_TRANSPORT", value: config.TransportTailscaleCLI, wantCode: 1},
		{name: "unknown", key: "HERDR_RELAY_TRANSPORT", value: "unknown", wantCode: 1},
		{name: "gateway conflict", key: "HERDR_GATEWAY_URL", value: "wss://gateway.example.test", wantCode: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := installedCommandEnv(t, config.TransportCloudflare)
			if test.key != "" {
				t.Setenv(test.key, test.value)
			}
			args := test.args
			if args == nil {
				args = []string{"serve-installed"}
			}
			code, err := runWithServerRunner(args, func(_ *app.Server, _ context.Context) error {
				t.Fatal("refused installed command reached the run lifecycle")
				return nil
			})
			if code != test.wantCode || err == nil {
				t.Fatalf("installed refusal = (%d, %v), want %d/error", code, err, test.wantCode)
			}
			for _, dir := range []string{"runtime", "cache", "config", "release", "data"} {
				if _, err := os.Lstat(filepath.Join(root, dir)); !os.IsNotExist(err) {
					t.Fatalf("refused public command touched %s: %v", dir, err)
				}
			}
		})
	}
}

func TestInstalledCommandRejectsNonOriginGatewaysBeforeEffects(t *testing.T) {
	for name, gateway := range map[string]string{
		"userinfo":       "wss://fixture-user:fixture-origin-canary@gateway.example.test",
		"path":           "wss://gateway.example.test/path",
		"escaped path":   "wss://gateway.example.test/%2F",
		"query":          "wss://gateway.example.test?private=fixture-origin-canary",
		"empty query":    "wss://gateway.example.test?",
		"fragment":       "wss://gateway.example.test#fixture-origin-canary",
		"empty fragment": "wss://gateway.example.test#",
		"remote WS":      "ws://gateway.example.test",
	} {
		for _, position := range []string{"leader", "fallback"} {
			t.Run(name+"/"+position, func(t *testing.T) {
				root := installedCommandEnv(t, config.TransportGateway)
				list := gateway
				if position == "fallback" {
					list = "wss://gateway.example.test," + gateway
				}
				t.Setenv("HERDR_GATEWAY_URL", list)
				code, err := runWithServerRunner([]string{"serve-installed"}, func(_ *app.Server, _ context.Context) error {
					t.Fatal("non-origin public gateway selection reached the run lifecycle")
					return nil
				})
				if code != 1 || err == nil {
					t.Fatal("direct public installed command admitted a non-origin gateway")
				}
				if strings.Contains(err.Error(), gateway) || strings.Contains(err.Error(), "fixture-origin-canary") {
					t.Fatal("public gateway refusal disclosed raw gateway input")
				}
				for _, dir := range []string{"runtime", "cache", "config", "release", "data"} {
					if _, err := os.Lstat(filepath.Join(root, dir)); !os.IsNotExist(err) {
						t.Fatal("invalid public gateway selection created app/store resources")
					}
				}
			})
		}
	}
}

func TestInstalledCommandConfigErrorsAreNonDisclosing(t *testing.T) {
	for name, gateway := range map[string]string{
		"unsupported scheme": "https://fixture-user:fixture-origin-canary@gateway.example.test",
		"malformed escape":   "wss://fixture-user:fixture-origin-canary@gateway.example.test/%zz",
		"missing host":       "wss:fixture-origin-canary",
	} {
		for _, position := range []string{"leader", "fallback"} {
			t.Run(name+"/"+position, func(t *testing.T) {
				root := installedCommandEnv(t, config.TransportGateway)
				list := gateway
				if position == "fallback" {
					list = "wss://gateway.example.test," + gateway
				}
				t.Setenv("HERDR_GATEWAY_URL", list)
				code, err := runWithServerRunner([]string{"serve-installed"}, func(_ *app.Server, _ context.Context) error {
					t.Fatal("invalid installed configuration reached the run lifecycle")
					return nil
				})
				if code != 1 || err == nil || err.Error() != "invalid installed service configuration" {
					t.Fatal("installed config failure did not return a fixed non-disclosing error")
				}
				if errors.Unwrap(err) != nil {
					t.Fatal("installed config refusal retained the raw configuration error")
				}
				for _, dir := range []string{"runtime", "cache", "config", "release", "data"} {
					if _, err := os.Lstat(filepath.Join(root, dir)); !os.IsNotExist(err) {
						t.Fatal("invalid installed configuration created app/store resources")
					}
				}
				output, fileErr := os.CreateTemp(root, "reported-error-*")
				if fileErr != nil {
					t.Fatal(fileErr)
				}
				defer output.Close()
				reportError(output, []string{"serve-installed"}, err)
				if _, fileErr := output.Seek(0, io.SeekStart); fileErr != nil {
					t.Fatal(fileErr)
				}
				data, fileErr := io.ReadAll(output)
				if fileErr != nil {
					t.Fatal(fileErr)
				}
				if !bytes.Contains(data, []byte("invalid installed service configuration")) ||
					bytes.Contains(data, []byte("fixture-origin-canary")) ||
					bytes.Contains(data, []byte("fixture-user")) || bytes.Contains(data, []byte(gateway)) {
					t.Fatal("reported installed configuration error is missing or disclosed gateway input")
				}
			})
		}
	}
}

func TestPublicCommandConfigErrorsAreNonDisclosing(t *testing.T) {
	for _, args := range [][]string{nil, {"serve"}, {"app-deploy-configured"}, {"support"}} {
		for name, gateway := range map[string]string{
			"unsupported scheme": "https://fixture-user:fixture-origin-canary@gateway.example.test",
			"malformed escape":   "wss://fixture-user:fixture-origin-canary@gateway.example.test/%zz",
			"missing host":       "wss:fixture-origin-canary",
		} {
			for _, position := range []string{"leader", "fallback"} {
				t.Run(fmtCommandArgs(args)+"/"+name+"/"+position, func(t *testing.T) {
					root := installedCommandEnv(t, config.TransportGateway)
					list := gateway
					if position == "fallback" {
						list = "wss://gateway.example.test," + gateway
					}
					t.Setenv("HERDR_GATEWAY_URL", list)
					t.Setenv("JOURNAL_STREAM", "")
					code, err := runWithServerRunner(args, func(_ *app.Server, _ context.Context) error {
						t.Fatal("invalid public configuration reached the run lifecycle")
						return nil
					})
					if code != 1 || err == nil || err.Error() != "invalid relay configuration" || errors.Unwrap(err) != nil {
						t.Fatal("public config failure did not return a fixed error without a raw cause")
					}
					for _, dir := range []string{"runtime", "cache", "config", "release", "data"} {
						if _, err := os.Lstat(filepath.Join(root, dir)); !os.IsNotExist(err) {
							t.Fatal("invalid public configuration created runtime resources")
						}
					}
					for _, format := range []string{"text", "json"} {
						t.Setenv("HERDR_RELAY_LOG_FORMAT", format)
						for _, journal := range []bool{false, true} {
							output, fileErr := os.CreateTemp(root, "public-reported-error-*")
							if fileErr != nil {
								t.Fatal(fileErr)
							}
							defer output.Close()
							if journal {
								reportErrorWithJournal(output, args, err, true)
							} else {
								reportError(output, args, err)
							}
							if _, fileErr := output.Seek(0, io.SeekStart); fileErr != nil {
								t.Fatal(fileErr)
							}
							data, fileErr := io.ReadAll(output)
							if fileErr != nil {
								t.Fatal(fileErr)
							}
							if !bytes.Contains(data, []byte("invalid relay configuration")) ||
								bytes.Contains(data, []byte("fixture-origin-canary")) ||
								bytes.Contains(data, []byte("fixture-user")) || bytes.Contains(data, []byte(gateway)) {
								t.Fatal("public error output is missing or disclosed raw gateway input")
							}
						}
					}
				})
			}
		}
	}
}

func TestForegroundCommandRequestedRearmStillReplacesBootstrap(t *testing.T) {
	for _, args := range [][]string{nil, {"serve"}} {
		t.Run(fmtCommandArgs(args), func(t *testing.T) {
			root := installedCommandEnv(t, config.TransportCloudflare)
			store, err := deviceauth.Open(filepath.Join(root, "runtime", "device-auth"))
			if err != nil {
				t.Fatal(err)
			}
			if err := store.EnsureBootstrapInvitation([]byte(strings.Repeat("k", 32)), "fixture", "en"); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "runtime", "device-auth", "devices.json")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("HERDR_RELAY_REARM_BOOTSTRAP", "true")
			called := false
			code, err := runWithServerRunner(args, func(_ *app.Server, _ context.Context) error {
				called = true
				after, err := os.ReadFile(path)
				if err != nil || bytes.Equal(before, after) {
					t.Fatal("foreground requested rearm did not replace the persisted invitation")
				}
				reopened, err := deviceauth.Open(filepath.Join(root, "runtime", "device-auth"))
				if err != nil || !reopened.BootstrapStatus().Armed {
					t.Fatal("foreground requested rearm did not leave a live bootstrap")
				}
				return nil
			})
			if code != 0 || err != nil || !called {
				t.Fatal("public config error redaction changed successful foreground rearm")
			}
		})
	}
}

func TestInstalledCommandRejectsRawRearmBeforeConfigOrEffects(t *testing.T) {
	for _, mode := range []string{config.TransportGateway, config.TransportCloudflare} {
		for _, value := range []string{"invalid", "true", "1", "True", "TRUE", "t", "T", " false ", "fixture-rearm-canary"} {
			t.Run(mode+"/"+value, func(t *testing.T) {
				root := installedCommandEnv(t, mode)
				t.Setenv("HERDR_RELAY_REARM_BOOTSTRAP", value)
				if value == "fixture-rearm-canary" {
					// Also prove raw policy validation precedes config errors;
					// the other cases use otherwise valid configuration.
					t.Setenv("HERDR_RELAY_TOKEN", "invalid-token-length")
				}
				code, err := runWithServerRunner([]string{"serve-installed"}, func(_ *app.Server, _ context.Context) error {
					t.Fatal("raw rearm refusal reached the run lifecycle")
					return nil
				})
				if code != 1 || err == nil || err.Error() != "installed service refuses bootstrap re-enrollment policy" {
					t.Fatal("installed command did not refuse raw rearm before configuration")
				}
				for _, dir := range []string{"runtime", "cache", "config", "release", "data"} {
					if _, err := os.Lstat(filepath.Join(root, dir)); !os.IsNotExist(err) {
						t.Fatal("raw rearm refusal created app/store resources")
					}
				}
			})
		}
	}
}

func TestInstalledCommandAcceptsOnlyFalseRearmSpellings(t *testing.T) {
	for _, mode := range []string{config.TransportGateway, config.TransportCloudflare} {
		for _, value := range []string{"", "0", "false", "False", "FALSE", "f", "F"} {
			t.Run(mode+"/"+value, func(t *testing.T) {
				root := installedCommandEnv(t, mode)
				t.Setenv("HERDR_RELAY_REARM_BOOTSTRAP", value)
				called := false
				code, err := runWithServerRunner([]string{"serve-installed"}, func(_ *app.Server, _ context.Context) error {
					called = true
					store, err := deviceauth.Open(filepath.Join(root, "runtime", "device-auth"))
					if err != nil || store.BootstrapStatus().Armed {
						t.Fatal("false rearm spelling failed authenticated non-arming startup")
					}
					return nil
				})
				if code != 0 || err != nil || !called {
					t.Fatal("installed command refused a wrapper-supported false rearm spelling")
				}
			})
		}
	}
}

func TestForegroundCommandMalformedRearmRetainsFallback(t *testing.T) {
	for _, args := range [][]string{nil, {"serve"}} {
		t.Run(fmtCommandArgs(args), func(t *testing.T) {
			root := installedCommandEnv(t, config.TransportCloudflare)
			t.Setenv("HERDR_RELAY_REARM_BOOTSTRAP", "invalid")
			called := false
			code, err := runWithServerRunner(args, func(_ *app.Server, _ context.Context) error {
				called = true
				store, err := deviceauth.Open(filepath.Join(root, "runtime", "device-auth"))
				if err != nil || !store.BootstrapStatus().Armed {
					t.Fatal("ordinary serve no longer ensures bootstrap with config's false fallback")
				}
				return nil
			})
			if code != 0 || err != nil || !called {
				t.Fatal("installed raw-policy validation changed ordinary/default serve")
			}
		})
	}
}

func TestInstalledCommandPropagatesRunnerFailure(t *testing.T) {
	installedCommandEnv(t, config.TransportCloudflare)
	failure := errors.New("fixture lifecycle failure")
	code, err := runWithServerRunner([]string{"serve-installed"}, func(_ *app.Server, _ context.Context) error { return failure })
	if code != 1 || !errors.Is(err, failure) {
		t.Fatalf("installed lifecycle failure = (%d, %v)", code, err)
	}
}

func TestPrintDevelopmentSetupLinkUsesBareVerifiedRelayOrigin(t *testing.T) {
	cfg := &config.Config{
		Token:              "0123456789abcdef0123456789abcdef",
		PhoneAppOrigin:     "https://app.example.test",
		TailscaleCLIOrigin: "https://relay.tailnet.ts.net:8443",
	}
	var output bytes.Buffer
	if err := printDevelopmentSetupLink(cfg, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "secret; do not share or log") {
		t.Fatalf("owner setup link output did not include the secret warning: %q", output.String())
	}
	var setupURL string
	for _, line := range strings.Split(output.String(), "\n") {
		if strings.HasPrefix(line, cfg.PhoneAppOrigin+"/#") {
			setupURL = line
			break
		}
	}
	parsedSetupURL, err := url.Parse(setupURL)
	if err != nil || parsedSetupURL.Scheme != "https" || parsedSetupURL.Host != "app.example.test" || parsedSetupURL.Path != "/" {
		t.Fatalf("printed setup URL = %q, parse error %v", setupURL, err)
	}
	fragment, err := url.ParseQuery(parsedSetupURL.Fragment)
	if err != nil || len(fragment["relay"]) != 1 {
		t.Fatalf("printed setup fragment has invalid relay fields: %q (%v)", parsedSetupURL.Fragment, err)
	}
	gotRelay := fragment.Get("relay")
	parsedRelay, err := url.Parse(gotRelay)
	configuredOrigin, originErr := url.Parse(cfg.TailscaleCLIOrigin)
	if err != nil || originErr != nil {
		t.Fatalf("parse relay/configured origins: relay error=%v origin error=%v", err, originErr)
	}
	wantRelay := (&url.URL{Scheme: "wss", Host: configuredOrigin.Host}).String()
	if gotRelay != wantRelay || parsedRelay.Scheme != "wss" || parsedRelay.Host != configuredOrigin.Host ||
		parsedRelay.User != nil || parsedRelay.Path != "" || parsedRelay.RawPath != "" ||
		parsedRelay.RawQuery != "" || parsedRelay.ForceQuery || parsedRelay.Fragment != "" {
		t.Fatalf("printed relay %q is not the bare configured WSS origin %q", gotRelay, wantRelay)
	}
	if fragment.Get("setup") != cfg.Token {
		t.Fatal("printed setup fragment does not preserve its setup token")
	}
}

func TestDevelopmentSetupLinkPrinterFixture(t *testing.T) {
	if os.Getenv("HERDR_TEST_DEV_SETUP_LINK_PRINTER") != "1" {
		t.Skip("hosted extracted-package fixture only")
	}
	cfg := &config.Config{
		Token:              os.Getenv("HERDR_TEST_DEV_SETUP_LINK_TOKEN"),
		PhoneAppOrigin:     os.Getenv("HERDR_TEST_DEV_SETUP_LINK_PHONE_ORIGIN"),
		TailscaleCLIOrigin: os.Getenv("HERDR_TEST_DEV_SETUP_LINK_RELAY_ORIGIN"),
	}
	outputPath := os.Getenv("HERDR_TEST_DEV_SETUP_LINK_OUTPUT")
	if !filepath.IsAbs(outputPath) {
		t.Fatal("fixture output path must be absolute")
	}
	var output bytes.Buffer
	if err := printDevelopmentSetupLink(cfg, &output); err != nil {
		t.Fatalf("production development setup-link printer failed: %v", err)
	}
	var setupURL string
	for _, line := range strings.Split(output.String(), "\n") {
		if strings.HasPrefix(line, cfg.PhoneAppOrigin+"/#") {
			setupURL = line
		}
	}
	if setupURL == "" {
		t.Fatal("production development setup-link printer emitted no app link")
	}
	file, err := os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatalf("create private fixture output: %v", err)
	}
	if _, err := io.WriteString(file, setupURL+"\n"); err != nil {
		_ = file.Close()
		t.Fatalf("write private fixture output: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close private fixture output: %v", err)
	}
}

func TestTailscaleCLICommandRefusesBeforeExecutableOrStateAccess(t *testing.T) {
	root := t.TempDir()
	invoked := filepath.Join(root, "cli-invoked")
	binary := filepath.Join(root, "fake-tailscale")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf invoked > '"+invoked+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(root, "registration")
	coordinationRoot := filepath.Join(root, "coordination")
	code, err := run([]string{
		"tailscale-cli", "status", "--binary", binary,
		"--state-root", stateRoot, "--coordination-root", coordinationRoot,
		"--scope", "production", "--installation-id", "fixture-installation",
		"--https-port", "8443", "--backend-port", "18377",
	})
	if code != 2 || !errors.Is(err, tailscalecli.ErrWorkflowRequired) {
		t.Fatalf("standalone CLI management command = (%d, %v), want workflow-required refusal", code, err)
	}
	for _, path := range []string{invoked, stateRoot, coordinationRoot} {
		if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
			t.Errorf("refused CLI command touched %q: %v", path, statErr)
		}
	}
}

func TestTailscaleCLIExecutionAndServerStartEntrypointInventory(t *testing.T) {
	base := t.TempDir()
	invoked := filepath.Join(base, "cli-invoked")
	binary := filepath.Join(base, "fake-tailscale")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf invoked > '"+invoked+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	entries := []struct {
		name string
		call func(*testing.T)
	}{
		{
			name: "standalone preflight refuses before executable contact",
			call: func(t *testing.T) {
				code, err := runTailscaleCLIWithInput([]string{"preflight", "--scope", "development", "--binary", binary}, strings.NewReader(""), io.Discard, io.Discard)
				if code != 2 || !errors.Is(err, tailscalecli.ErrWorkflowRequired) {
					t.Fatalf("standalone preflight = (%d, %v), want workflow-required refusal", code, err)
				}
			},
		},
		{
			name: "development preflight requires validated isolation",
			call: func(t *testing.T) {
				t.Setenv("HERDR_TAILSCALE_CLI_BIN", binary)
				code, err := runDevelopmentTailscaleCLI([]string{"preflight"}, strings.NewReader(""), io.Discard, io.Discard)
				if code == 0 || err == nil {
					t.Fatalf("development preflight admitted an unbound environment: (%d, %v)", code, err)
				}
			},
		},
		{
			name: "development status requires validated isolation",
			call: func(t *testing.T) {
				t.Setenv("HERDR_TAILSCALE_CLI_BIN", binary)
				code, err := runDevelopmentTailscaleCLI([]string{"status"}, strings.NewReader(""), io.Discard, io.Discard)
				if code == 0 || err == nil {
					t.Fatalf("development status admitted an unbound environment: (%d, %v)", code, err)
				}
			},
		},
		{
			name: "ordinary config load cannot start CLI transport",
			call: func(t *testing.T) {
				configureUnboundDevelopmentCLIConfig(t, base, binary)
				if _, err := config.Load(); !errors.Is(err, tailscalecli.ErrWorkflowRequired) {
					t.Fatalf("ordinary config load = %v, want workflow-required refusal", err)
				}
			},
		},
		{
			name: "ordinary serve entrypoint cannot start CLI transport",
			call: func(t *testing.T) {
				configureUnboundDevelopmentCLIConfig(t, base, binary)
				code, err := runServe()
				if code == 0 || !errors.Is(err, tailscalecli.ErrWorkflowRequired) {
					t.Fatalf("serve entrypoint = (%d, %v), want workflow-required refusal", code, err)
				}
			},
		},
		{
			name: "app New refuses CLI server start without workflow",
			call: func(t *testing.T) {
				server := app.New(&config.Config{Transport: config.TransportTailscaleCLI}, "test", "test", nil)
				if err := server.Run(context.Background()); !errors.Is(err, tailscalecli.ErrWorkflowRequired) {
					t.Fatalf("app.New server start = %v, want workflow-required refusal", err)
				}
			},
		},
		{
			name: "app NewOwned refuses CLI server start",
			call: func(t *testing.T) {
				if _, err := app.NewOwned(&config.Config{Transport: config.TransportTailscaleCLI}, "test", "test", nil, nil); !errors.Is(err, tailscalecli.ErrWorkflowRequired) {
					t.Fatalf("app.NewOwned = %v, want workflow-required refusal", err)
				}
			},
		},
		{
			name: "app development constructor requires workflow",
			call: func(t *testing.T) {
				if _, err := app.NewDevelopmentCLI(&config.Config{Transport: config.TransportTailscaleCLI}, "test", "test", nil, nil); !errors.Is(err, tailscalecli.ErrWorkflowRequired) {
					t.Fatalf("app.NewDevelopmentCLI = %v, want workflow-required refusal", err)
				}
			},
		},
		{
			name: "binary resolution is filesystem-only",
			call: func(t *testing.T) {
				var stdout, stderr bytes.Buffer
				code, err := runTailscaleCLIWithInput([]string{"resolve-binary", "--binary", binary}, strings.NewReader(""), &stdout, &stderr)
				resolved, resolveErr := filepath.EvalSymlinks(binary)
				if code != 0 || err != nil || resolveErr != nil || strings.TrimSpace(stdout.String()) != resolved {
					t.Fatalf("binary resolution = (%d, %v, %q), canonical=%q resolveErr=%v", code, err, stdout.String(), resolved, resolveErr)
				}
			},
		},
	}

	for _, entry := range entries {
		t.Run(entry.name, func(t *testing.T) {
			entry.call(t)
			if _, err := os.Lstat(invoked); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("entrypoint contacted the Tailscale executable: %v", err)
			}
		})
	}
}

func configureUnboundDevelopmentCLIConfig(t *testing.T, base, binary string) {
	t.Helper()
	for name, value := range map[string]string{
		"HERDR_RELAY_TRANSPORT":                 "tailscale-cli",
		"HERDR_RELAY_TOKEN":                     "0123456789abcdef0123456789abcdef",
		"HERDR_RELAY_HOST":                      "127.0.0.1",
		"HERDR_RELAY_PORT":                      "18377",
		"HERDR_RELAY_PLUGIN_PORT":               "18378",
		"HERDR_RELAY_INSTANCE_ID":               "fixture-installation",
		"HERDR_RELAY_CONTROL_RUN_ID":            "fixture-control-run",
		"HERDR_RELAY_PAIRING_SOCKET":            filepath.Join(base, "pairing.sock"),
		"HERDR_TAILSCALE_CLI_ORIGIN":            "https://relay.fixture.invalid:8443",
		"HERDR_TAILSCALE_CLI_SCOPE":             "development",
		"HERDR_TAILSCALE_CLI_BIN":               binary,
		"HERDR_TAILSCALE_CLI_STATE_ROOT":        filepath.Join(base, "development", "registration"),
		"HERDR_TAILSCALE_CLI_COORDINATION_ROOT": filepath.Join(base, "coordination"),
		"HERDR_TAILSCALE_CLI_DEVELOPMENT_ROOT":  filepath.Join(base, "development"),
		"HERDR_TAILSCALE_CLI_HTTPS_PORT":        "8443",
		"HERDR_PHONE_APP_URL":                   "https://app.fixture.invalid",
		"HERDR_REACHABILITY_PORT_MAPPING":       "0",
		"HERDR_RELAY_REARM_BOOTSTRAP":           "0",
		"HERDR_RELAY_ENV":                       filepath.Join(base, "relay.env"),
		"XDG_CONFIG_HOME":                       filepath.Join(base, "config"),
		"XDG_CACHE_HOME":                        filepath.Join(base, "cache"),
		"XDG_DATA_HOME":                         filepath.Join(base, "data"),
	} {
		t.Setenv(name, value)
	}
}

func TestVerifyReleaseIdentity(t *testing.T) {
	originalVersion, originalRevision := version, revision
	version, revision = "1.2.3", "candidate-revision"
	t.Cleanup(func() {
		version, revision = originalVersion, originalRevision
	})

	manifest := release.Manifest{
		Version:  "1.2.3",
		Revision: "candidate-revision",
		Target:   release.CurrentTarget(),
	}
	if err := verifyReleaseIdentity(manifest, "1.2.3", "candidate-revision", release.CurrentTarget(), false); err != nil {
		t.Fatalf("matching identity rejected: %v", err)
	}

	tests := []struct {
		name             string
		manifest         release.Manifest
		expectedVersion  string
		expectedRevision string
		expectedTarget   string
		allowCrossTarget bool
		errorPart        string
	}{
		{
			name:             "workflow version",
			manifest:         manifest,
			expectedVersion:  "1.2.4",
			expectedRevision: "candidate-revision",
			expectedTarget:   release.CurrentTarget(),
			errorPart:        "expected version",
		},
		{
			name:             "workflow revision",
			manifest:         manifest,
			expectedVersion:  "1.2.3",
			expectedRevision: "other-revision",
			expectedTarget:   release.CurrentTarget(),
			errorPart:        "expected revision",
		},
		{
			name:             "workflow target",
			manifest:         manifest,
			expectedVersion:  "1.2.3",
			expectedRevision: "candidate-revision",
			expectedTarget:   "other/target",
			errorPart:        "expected target",
		},
		{
			name: "binary version",
			manifest: release.Manifest{
				Version:  "1.2.4",
				Revision: "candidate-revision",
				Target:   release.CurrentTarget(),
			},
			expectedTarget: release.CurrentTarget(),
			errorPart:      "binary version",
		},
		{
			name: "binary revision",
			manifest: release.Manifest{
				Version:  "1.2.3",
				Revision: "other-revision",
				Target:   release.CurrentTarget(),
			},
			expectedTarget: release.CurrentTarget(),
			errorPart:      "binary revision",
		},
		{
			name: "binary target",
			manifest: release.Manifest{
				Version:  "1.2.3",
				Revision: "candidate-revision",
				Target:   "other/target",
			},
			expectedTarget: "other/target",
			errorPart:      "binary target",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := verifyReleaseIdentity(
				test.manifest,
				test.expectedVersion,
				test.expectedRevision,
				test.expectedTarget,
				test.allowCrossTarget,
			)
			if err == nil || !strings.Contains(err.Error(), test.errorPart) {
				t.Fatalf("identity error = %v, want %q", err, test.errorPart)
			}
		})
	}

	crossTarget := manifest
	crossTarget.Target = "other/target"
	if err := verifyReleaseIdentity(crossTarget, "", "", "other/target", true); err != nil {
		t.Fatalf("cross-target build-host verification rejected: %v", err)
	}
}

// stampedWebCopy copies the tracked web tree into a test-owned directory and stamps the
// copy's version.json with release_version and revision, as scripts/stamp-web-version.mjs
// does when a release is packaged. The tracked version.json carries only `version`: the
// packaged identity does not exist until packaging, so the fixture must not read it from
// the tracked tree. The tracked files are never modified.
func stampedWebCopy(t *testing.T, source string) string {
	t.Helper()
	destination := filepath.Join(t.TempDir(), "web")
	err := filepath.Walk(source, func(name string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, name)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !info.Mode().IsRegular() {
			return errors.New("web fixture contains a non-regular file: " + relative)
		}
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	versionPath := filepath.Join(destination, "version.json")
	data, err := os.ReadFile(versionPath)
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(data, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata["version"] == nil {
		t.Fatal("tracked web version metadata has no version")
	}
	metadata["release_version"] = metadata["version"]
	revision, err := json.Marshal("0123456789abcdef0123456789abcdef01234567")
	if err != nil {
		t.Fatal(err)
	}
	metadata["revision"] = revision
	stamped, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	stamped = append(stamped, '\n')
	if err := os.WriteFile(versionPath, stamped, 0o644); err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	encoder := brotli.NewWriterLevel(&compressed, 11)
	if _, err := encoder.Write(stamped); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(versionPath+".br", compressed.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return destination
}

// canonicalPagesWebHandler serves the stamped web root the way Pages canonically
// serves it to the app verifier: the exact build entry (.../index.html) is
// normalized to its directory with one same-origin 308 that keeps any query, the
// directory serves the entry bytes as HTML, and every other file is served
// unchanged. Go's bare http.FileServer answers that index.html with a 301
// instead, which the verifier admits only as the single 308 hop.
func canonicalPagesWebHandler(t *testing.T, webRoot string) http.Handler {
	t.Helper()
	descriptor, err := release.LoadWebDescriptor(os.DirFS(webRoot))
	if err != nil {
		t.Fatal(err)
	}
	entry := descriptor.Entry
	if !strings.HasPrefix(entry, "/builds/") || !strings.HasSuffix(entry, "/index.html") {
		t.Fatal("packaged build entry is not a build index.html")
	}
	directory := strings.TrimSuffix(entry, "index.html")
	files := http.FileServer(http.Dir(webRoot))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != entry {
			files.ServeHTTP(w, r)
			return
		}
		location := directory
		if r.URL.RawQuery != "" {
			location += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, location, http.StatusPermanentRedirect)
	})
}

// A fresh subprocess keeps the trust pool independent of other unit tests.
// A run-owned root stands in for OS trust on both hosted platforms; the actual
// command still uses default verified TLS, never an insecure client. The only
// HTTPS endpoint is this run-owned disposable loopback fixture;
// no account, external network, real service or Tailscale is contacted.
func TestVerifyPublicExactTrustedGatewayApp(t *testing.T) {
	if os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("RUNNER_ENVIRONMENT") != "github-hosted" {
		t.Skip("exact trusted app subprocess fixture is hosted-only")
	}
	sourceRoot, err := filepath.Abs(filepath.Join("..", "..", "web"))
	if err != nil {
		t.Fatal(err)
	}
	webRoot := stampedWebCopy(t, sourceRoot)
	hash, err := release.WebHashFS(os.DirFS(webRoot))
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := os.ReadFile(filepath.Join(webRoot, "version.json"))
	if err != nil {
		t.Fatal(err)
	}
	var identity struct {
		Version  string `json:"release_version"`
		Revision string `json:"revision"`
	}
	if err := json.Unmarshal(metadata, &identity); err != nil || identity.Version == "" || identity.Revision == "" {
		t.Fatal("packaged fixture identity is missing")
	}
	server := httptest.NewTLSServer(canonicalPagesWebHandler(t, webRoot))
	defer server.Close()
	root := t.TempDir()
	cert := filepath.Join(root, "fixture-ca.pem")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	trustDir := filepath.Join(root, "empty-trust-dir")
	if err := os.Mkdir(trustDir, 0o700); err != nil {
		t.Fatal(err)
	}
	args, err := json.Marshal([]string{"verify-public", "--web-root", webRoot, "--origin", server.URL,
		"--version", identity.Version, "--revision", identity.Revision, "--web-hash", hash})
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestGatewayAppVerifierSubprocess$")
	cmd.Env = []string{"HOME=" + root, "PATH=/usr/bin:/bin", "SSL_CERT_FILE=" + cert, "SSL_CERT_DIR=" + trustDir,
		"GODEBUG=x509usefallbackroots=1", "HERDR_TEST_GATEWAY_APP_CA=" + cert,
		"GITHUB_ACTIONS=true", "RUNNER_ENVIRONMENT=github-hosted", "HERDR_TEST_GATEWAY_APP_ARGS=" + string(args)}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("exact trusted packaged app refused: %v (%s)", err, output)
	}
}

func TestGatewayAppVerifierSubprocess(t *testing.T) {
	text := os.Getenv("HERDR_TEST_GATEWAY_APP_ARGS")
	if text == "" {
		return
	}
	if os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("RUNNER_ENVIRONMENT") != "github-hosted" {
		t.Fatal("app verifier subprocess must be hosted")
	}
	cert, err := os.ReadFile(os.Getenv("HERDR_TEST_GATEWAY_APP_CA"))
	if err != nil {
		t.Fatal("fixture trust root unavailable")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(cert) {
		t.Fatal("invalid fixture trust root")
	}
	x509.SetFallbackRoots(pool)
	var args []string
	if err := json.Unmarshal([]byte(text), &args); err != nil {
		t.Fatal("invalid app fixture argv")
	}
	code, err := run(args)
	if err != nil {
		// The verifier's own error is already sanitized: it names the refused
		// resource and rule, never a credential or a response body.
		t.Fatalf("app verifier refused the fixture: %v", err)
	}
	if code != 0 {
		t.Fatalf("app verifier did not pass (exit %d)", code)
	}
}

func TestVerifyPublicPackagedHashRefusesBeforeNetwork(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "version.json"), []byte(`{"release_version":"version","revision":"revision"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, hash := range []string{"foreign", "different-build-same-release"} {
		code, err := run([]string{"verify-public", "--web-root", root, "--origin", "https://unreachable.invalid", "--web-hash", hash})
		if code != 1 || err == nil || err.Error() != "phone-app bundle does not match the packaged relay identity" {
			t.Fatalf("hash mismatch was not refused before network: (%d, %v)", code, err)
		}
	}
}

func TestVerifyGatewayHealthCommand(t *testing.T) {
	valid := `{"status":"ok","readiness":"ready","transport":"gateway","inventory":{"state":"ready","stale":false},"instance":"instance","release_version":"version","revision":"revision","bundle_hash":"web","bundle_version":"version","bundle_revision":"revision","protocol":3,"gateway":{"enabled":true,"registered":true,"relay_id":"fixture"}}`
	for _, test := range []struct {
		name string
		body string
		args []string
		code int
	}{
		{"ready", valid, []string{"instance", "version", "revision", "web"}, 0},
		{"foreign", valid, []string{"foreign", "version", "revision", "web"}, 1},
		{"unregistered", strings.Replace(valid, `"registered":true`, `"registered":false`, 1), []string{"instance", "version", "revision", "web"}, 1},
		{"malformed redacted", `{"private":"secret-canary",`, []string{"instance", "version", "revision", "web"}, 1},
		{"usage", valid, nil, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			input, err := os.CreateTemp(t.TempDir(), "health")
			if err != nil {
				t.Fatal(err)
			}
			defer input.Close()
			if _, err := input.WriteString(test.body); err != nil {
				t.Fatal(err)
			}
			if _, err := input.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			previous := os.Stdin
			os.Stdin = input
			defer func() { os.Stdin = previous }()
			code, runErr := run(append([]string{"verify-gateway-health"}, test.args...))
			if code != test.code || (runErr == nil) != (test.code == 0) {
				t.Fatalf("command = (%d, %v), want code %d", code, runErr, test.code)
			}
			if runErr != nil && strings.Contains(runErr.Error(), "secret-canary") {
				t.Fatal("gateway verifier exposed its input")
			}
		})
	}
}

func TestCheckPortSupportsUDP(t *testing.T) {
	listener, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(listener.LocalAddr().(*net.UDPAddr).Port)
	code, err := run([]string{"check-port", "--host", "127.0.0.1", "--port", port, "--protocol", "udp"})
	if code == 0 || err == nil {
		t.Fatalf("occupied UDP port accepted: code=%d err=%v", code, err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	code, err = run([]string{"check-port", "--host", "127.0.0.1", "--port", port, "--protocol", "udp"})
	if code != 0 || err != nil {
		t.Fatalf("free UDP port rejected: code=%d err=%v", code, err)
	}
}

func TestVerifyReleaseRejectsCrossTargetCandidateMode(t *testing.T) {
	for _, candidateFlag := range []string{"--version", "--revision"} {
		t.Run(candidateFlag, func(t *testing.T) {
			code, err := run([]string{"verify-release", "--allow-cross-target", candidateFlag, "candidate"})
			if code != 2 || err == nil || !strings.Contains(err.Error(), "cannot be combined") {
				t.Fatalf("run() = (%d, %v), want usage error", code, err)
			}
		})
	}
}

func TestPairingControlKeepsDecodedNegativeRepliesDistinctFromTransportFailure(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "relay-control-cli.")
	if err != nil {
		t.Fatal("create private pairing-control fixture")
	}
	defer os.RemoveAll(root)

	const runID = "run-negative-cli"
	const instance = "instance-negative-cli"
	const fixtureSecret = "fixture-private-authorization-proof"
	socket := filepath.Join(root, "control.sock")
	server, err := localcontrol.NewManaged(socket, runID, instance, localcontrol.Callbacks{
		Status: func(context.Context) localcontrol.Status {
			return localcontrol.Status{OwnerHeld: true, RunID: runID, Instance: instance}
		},
		Activate: func(context.Context) (localcontrol.Status, error) {
			return localcontrol.Status{RegistrationOutcome: "settled-no-write"}, errors.New(fixtureSecret)
		},
		Arm: func(context.Context) (localcontrol.Status, error) {
			return localcontrol.Status{ArmOutcome: "not-committed"}, errors.New(fixtureSecret)
		},
	})
	if err != nil {
		t.Fatal("create pairing-control fixture")
	}
	ctx, cancel := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := server.Close(); err != nil {
			t.Error("close pairing-control fixture")
		}
		<-serverDone
	})

	for _, test := range []struct {
		name      string
		operation string
		outcome   string
	}{
		{name: "activation", operation: "activate", outcome: "settled-no-write"},
		{name: "arm", operation: "arm_bootstrap", outcome: "not-committed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := []string{"pairing-control", "--socket", socket, "--operation", test.operation, "--run-id", runID, "--instance", instance}
			code, runErr, output := captureRunStdout(t, args)
			if code != 0 || runErr != nil {
				t.Fatal("decoded negative pairing-control result was treated as a transport failure")
			}
			var response localcontrol.Response
			if err := json.Unmarshal(output, &response); err != nil {
				t.Fatal("pairing-control did not emit a JSON reply")
			}
			if response.OK || response.Error == "" || response.RunID != runID || response.Instance != instance || strings.Contains(response.Error, fixtureSecret) {
				t.Fatal("decoded negative pairing-control response lost its identity or exposed private error detail")
			}
			if test.name == "activation" && response.RegistrationOutcome != test.outcome {
				t.Fatal("activation negative outcome was not preserved")
			}
			if test.name == "arm" && response.ArmOutcome != test.outcome {
				t.Fatal("arm negative outcome was not preserved")
			}
			if strings.Contains(string(output), fixtureSecret) {
				t.Fatal("private authorization proof leaked into decoded response")
			}
		})
	}

	missingSocket := filepath.Join(root, "missing.sock")
	code, runErr, output := captureRunStdout(t, []string{"pairing-control", "--socket", missingSocket, "--operation", "activate", "--run-id", runID, "--instance", instance})
	if code == 0 || runErr == nil || len(output) != 0 {
		t.Fatal("transport failure was not kept distinct from a decoded negative reply")
	}
}

func captureRunStdout(t *testing.T, args []string) (int, error, []byte) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal("create pairing-control output capture")
	}
	previous := os.Stdout
	os.Stdout = writer
	defer func() { os.Stdout = previous }()
	code, runErr := run(args)
	if err := writer.Close(); err != nil {
		t.Fatal("close pairing-control output capture")
	}
	os.Stdout = previous
	output, err := io.ReadAll(reader)
	if closeErr := reader.Close(); closeErr != nil {
		t.Fatal("close pairing-control output reader")
	}
	if err != nil {
		t.Fatal("read pairing-control output")
	}
	return code, runErr, output
}

// The private mount-guard CLI contract: fixed exit codes and messages, and no
// caller-supplied pathname or syscall detail in any output. Every input here is
// refused on every platform (the Linux proof is in internal/uninstallmount).
func TestPrivateUninstallMountGuardContract(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const secret = "/fixture-private-canary"
	for _, tc := range []struct {
		name string
		args []string
		code int
	}{
		{"strict, no roots", []string{"__private-uninstall-mount-guard"}, 2},
		{"strict, two roots", []string{"__private-uninstall-mount-guard", "a", secret}, 2},
		{"strict, four roots", []string{"__private-uninstall-mount-guard", "a", "b", "c", secret}, 2},
		{"strict, foreign root", []string{"__private-uninstall-mount-guard", "a", "b", secret}, 1},
		{"roots, none", []string{"__private-uninstall-mount-roots"}, 2},
		{"roots, four", []string{"__private-uninstall-mount-roots", "a", "b", "c", secret}, 2},
		{"roots, foreign root", []string{"__private-uninstall-mount-roots", secret}, 1},
		{"roots, three foreign roots", []string{"__private-uninstall-mount-roots", "a", "b", secret}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, err := run(tc.args)
			if err == nil || strings.Contains(err.Error(), secret) {
				t.Fatalf("expected a fixed refusal, got %v", err)
			}
			if code != tc.code {
				t.Fatalf("exit %d, want %d (%v)", code, tc.code, err)
			}
			if tc.code == 1 && err.Error() != "uninstall mount guard refused" {
				t.Fatalf("refusal is not the fixed message: %v", err)
			}
		})
	}
}
