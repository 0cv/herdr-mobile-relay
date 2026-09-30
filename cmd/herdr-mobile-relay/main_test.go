package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/app"
	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/localcontrol"
	"github.com/0cv/herdr-mobile-relay/internal/release"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

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
