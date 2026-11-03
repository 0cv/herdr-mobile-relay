package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackendDefaults(t *testing.T) {
	for _, backend := range []string{"herdr", "hc"} {
		t.Run(backend, func(t *testing.T) {
			isolateLoadEnvironment(t)
			if backend == "hc" {
				t.Setenv("HERDR_RELAY_BACKEND", backend)
				t.Setenv("HERDR_SOCKET_PATH", "/stale/tui.sock")
				t.Setenv("HERDR_PLUGIN_CONFIG_DIR", "/tui/plugin")
			}
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			name, port, service := "herdr-mobile-relay", 8375, defaultServiceName()
			socket := filepath.Join(cfg.ConfigHome, "herdr", "herdr.sock")
			if backend == "hc" {
				name += "-hc"
				port = 8377
				service = strings.TrimSuffix(service, ".service") + "-hc.service"
				socket = filepath.Join(os.Getenv("HOME"), ".local/state/herdr-core/default/default.sock")
			}
			if cfg.Backend != backend || cfg.Port != port || cfg.PluginPort != port+1 || cfg.ServiceName != service ||
				cfg.SocketPath != socket || cfg.RuntimeDir != filepath.Join(cfg.ConfigHome, name) ||
				cfg.CacheDir != filepath.Join(os.Getenv("XDG_CACHE_HOME"), name) {
				t.Fatalf("unexpected config: %+v", cfg)
			}
		})
	}
}

func TestHCOverrides(t *testing.T) {
	isolateLoadEnvironment(t)
	t.Setenv("HERDR_RELAY_BACKEND", "hc")
	t.Setenv("XDG_STATE_HOME", "/state")
	t.Setenv("HC_SESSION", "second")
	t.Setenv("HERDR_RELAY_PORT", "9001")
	t.Setenv("HERDR_RELAY_PLUGIN_PORT", "9002")
	t.Setenv("HERDR_RELAY_SERVICE_NAME", "custom.service")
	t.Setenv("HERDR_RELAY_ENV", "/runtime/core/relay.env")
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", "/tui/plugin")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SocketPath != "/state/herdr-core/second/second.sock" || cfg.Port != 9001 || cfg.PluginPort != 9002 ||
		cfg.ServiceName != "custom.service" || cfg.RuntimeDir != "/runtime/core" {
		t.Fatalf("%+v", cfg)
	}
	t.Setenv("HC_SOCKET_PATH", "/explicit/core.sock")
	t.Setenv("HERDR_SOCKET_PATH", "/stale/tui.sock")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SocketPath != "/explicit/core.sock" {
		t.Fatal(cfg.SocketPath)
	}
}

func TestInvalidBackend(t *testing.T) {
	isolateLoadEnvironment(t)
	t.Setenv("HERDR_RELAY_BACKEND", "other")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "HERDR_RELAY_BACKEND") || !strings.Contains(err.Error(), "herdr or hc") {
		t.Fatalf("unexpected error: %v", err)
	}
}
