package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/app"
	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

func installedCLICommandEnv(t *testing.T) string {
	t.Helper()
	root := installedCommandEnv(t, config.TransportTailscaleCLI)
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{
		"HOME": root, "XDG_CONFIG_HOME": filepath.Join(root, "config"),
		"XDG_CACHE_HOME": filepath.Join(root, "cache"), "XDG_DATA_HOME": filepath.Join(root, "data"),
		"HERDR_RELAY_ENV":    filepath.Join(root, "config", "relay", "relay.env"),
		"HERDR_RELEASE_ROOT": filepath.Join(root, "release"), "HERDR_WEB_ROOT": filepath.Join(root, "web"),
		"HERDR_BIN": filepath.Join(root, "absent-herdr"), "HERDR_SOCKET_PATH": filepath.Join(root, "absent-socket"),
		"HERDR_REACHABILITY_PORT_MAPPING": "0", "HERDR_TAILSCALE_CLI_SCOPE": "production",
		"HERDR_TAILSCALE_CLI_ORIGIN":            "https://herdr.tailnet.ts.net:9443",
		"HERDR_PHONE_APP_URL":                   "https://herdr.tailnet.ts.net:9443",
		"HERDR_TAILSCALE_CLI_HTTPS_PORT":        "9443",
		"HERDR_TAILSCALE_CLI_STATE_ROOT":        filepath.Join(root, "registration"),
		"HERDR_TAILSCALE_CLI_COORDINATION_ROOT": filepath.Join(root, "coordination"),
		"HERDR_TAILSCALE_CLI_BIN":               filepath.Join(root, "absent-tailscale"),
		"HERDR_RELAY_PORT":                      "9375", "HERDR_RELAY_PLUGIN_PORT": "9376",
	} {
		t.Setenv(key, value)
	}
	return root
}

func dispatchInstalledCLI(t *testing.T) (int, error) {
	t.Helper()
	return runCommand([]string{"serve-installed"}, func(*app.Server, context.Context) error {
		t.Fatal("installed CLI used legacy runner")
		return nil
	}, func(context.Context) error {
		t.Fatal("installed CLI swept services")
		return nil
	})
}

// Runs in ordinary builds too: valid installed-only ports/identity load as
// data, then refuse before executable resolution or any resource construction.
func TestInstalledCLICommandUnavailable(t *testing.T) {
	root := installedCLICommandEnv(t)
	if config.TailscaleCLIProfilesEnabled() {
		t.Fatal("production enabled")
	}
	cfg, err := config.LoadInstalled()
	if err != nil || cfg.Port != 9375 {
		t.Fatalf("installed load: %v", err)
	}
	selected, err := config.BindInstalledCLI(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tailscalecli.NewInstalledManager(selected.Binding()); err == nil {
		t.Fatal("concrete constructor enabled production")
	}
	if _, err := config.Load(); err == nil {
		t.Fatal("ordinary load admitted installed CLI")
	}
	code, err := dispatchInstalledCLI(t)
	if code != 1 || err != errInstalledCLIUnavailable {
		t.Fatalf("refusal: %d %v", code, err)
	}
	for _, leaf := range []string{"config", "cache", "registration", "coordination"} {
		if _, err := os.Lstat(filepath.Join(root, leaf)); !os.IsNotExist(err) {
			t.Fatalf("resource created: %s %v", leaf, err)
		}
	}
}

func TestInstalledCLICommandInvalidPolicy(t *testing.T) {
	for _, pair := range [][2]string{
		{"HERDR_RELAY_REARM_BOOTSTRAP", "secret-canary"},
		{"HERDR_RELAY_CONTROL_RUN_ID", "secret-canary"},
		{"HERDR_RELAY_RUN_ID", "secret-canary"},
		{"HERDR_RELAY_PAIRING_SOCKET", "/secret-canary"},
		{"HERDR_TAILSCALE_CLI_SCOPE", "development"},
		{"HERDR_TAILSCALE_CLI_DEVELOPMENT_ROOT", "/secret-canary"},
		{"HERDR_REACHABILITY_PORT_MAPPING", "typo"},
		{"HERDR_GATEWAY_SELECTION", "typo"},
		{"HERDR_RELAY_PORT", "typo"},
		{"HERDR_TAILSCALE_CLI_HTTPS_PORT", "8443"},
	} {
		t.Run(pair[0], func(t *testing.T) {
			root := installedCLICommandEnv(t)
			t.Setenv(pair[0], pair[1])
			if _, err := config.LoadInstalled(); err == nil {
				t.Fatal("malformed installed data accepted")
			}
			code, err := dispatchInstalledCLI(t)
			want := "invalid installed service configuration"
			if pair[0] == "HERDR_RELAY_REARM_BOOTSTRAP" {
				want = "installed service refuses bootstrap re-enrollment policy"
			}
			if code != 1 || err == nil || err.Error() != want {
				t.Fatalf("refusal: %d %v", code, err)
			}
			if _, err := os.Lstat(filepath.Join(root, "config")); !os.IsNotExist(err) {
				t.Fatal("created runtime")
			}
		})
	}
}
