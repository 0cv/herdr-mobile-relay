package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	os.Unsetenv("HERDR_RELAY_HOST")
	os.Unsetenv("HERDR_RELAY_PORT")
	os.Unsetenv("HERDR_RELAY_TOKEN")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "127.0.0.1" {
		t.Errorf("host = %q, want 127.0.0.1", cfg.Host)
	}
	if cfg.Port != 8375 {
		t.Errorf("port = %d, want 8375", cfg.Port)
	}
	if cfg.PluginPort != 8376 {
		t.Errorf("plugin port = %d, want 8376", cfg.PluginPort)
	}
	if cfg.PollInterval != 2.0 {
		t.Errorf("poll interval = %f, want 2.0", cfg.PollInterval)
	}
}

func TestLoadRejectsTokenlessNonLoopback(t *testing.T) {
	t.Setenv("HERDR_RELAY_HOST", "0.0.0.0")
	t.Setenv("HERDR_RELAY_TOKEN", "")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for tokenless non-loopback bind")
	}
}

func TestLoadRejectsShortRelayKey(t *testing.T) {
	t.Setenv("HERDR_RELAY_TOKEN", "predictable")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for relay key shorter than 16 bytes")
	}
}

func TestLoadAllowedOrigins(t *testing.T) {
	t.Setenv("HERDR_ALLOWED_ORIGINS", "https://a.com, https://b.com ,")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.AllowedOrigins) != 2 {
		t.Fatalf("origins = %v, want 2 entries", cfg.AllowedOrigins)
	}
	if cfg.AllowedOrigins[0] != "https://a.com" || cfg.AllowedOrigins[1] != "https://b.com" {
		t.Errorf("origins = %v", cfg.AllowedOrigins)
	}
}

func TestLoadIsolatesAllXDGPaths(t *testing.T) {
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	cacheHome := filepath.Join(root, "cache")
	dataHome := filepath.Join(root, "data")
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_CACHE_HOME", cacheHome)
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("HERDR_RELAY_ENV", "")
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", "")
	t.Setenv("HERDR_RELEASE_ROOT", "")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConfigHome != configHome {
		t.Fatalf("config home = %q, want %q", cfg.ConfigHome, configHome)
	}
	if cfg.CacheDir != filepath.Join(cacheHome, "herdr-mobile-relay") {
		t.Fatalf("cache dir = %q", cfg.CacheDir)
	}
	if cfg.DataHome != dataHome {
		t.Fatalf("data home = %q, want %q", cfg.DataHome, dataHome)
	}
	if cfg.RuntimeDir != filepath.Join(configHome, "herdr-mobile-relay") {
		t.Fatalf("runtime dir = %q", cfg.RuntimeDir)
	}
	if cfg.ReleaseRoot != filepath.Join(dataHome, "herdr-mobile-relay") {
		t.Fatalf("release root = %q", cfg.ReleaseRoot)
	}
}

func TestLoadGatewayDefaults(t *testing.T) {
	t.Setenv("HERDR_GATEWAY_URL", "")
	t.Setenv("HERDR_WEBRTC_UDP_PORT", "")
	t.Setenv("HERDR_TRANSPORT_FORCE_RELAY", "")
	t.Setenv("HERDR_REACHABILITY_PORT_MAPPING", "")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GatewayURL != "" {
		t.Errorf("gateway url = %q, want empty", cfg.GatewayURL)
	}
	if cfg.WebRTCUDPPort != 0 {
		t.Errorf("webrtc udp port = %d, want 0", cfg.WebRTCUDPPort)
	}
	if cfg.ForceRelayTransport {
		t.Error("force relay transport = true, want false")
	}
	if !cfg.PortMappingEnabled {
		t.Error("port mapping enabled = false, want true")
	}
}

func TestLoadGatewaySettings(t *testing.T) {
	t.Setenv("HERDR_RELAY_TOKEN", "0123456789abcdef0123456789abcdef")
	t.Setenv("HERDR_GATEWAY_URL", "wss://gw.example.com/")
	t.Setenv("HERDR_WEBRTC_UDP_PORT", "41234")
	t.Setenv("HERDR_TRANSPORT_FORCE_RELAY", "true")
	t.Setenv("HERDR_REACHABILITY_PORT_MAPPING", "false")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GatewayURL != "wss://gw.example.com" {
		t.Errorf("gateway url = %q, want trailing slash trimmed", cfg.GatewayURL)
	}
	if cfg.WebRTCUDPPort != 41234 {
		t.Errorf("webrtc udp port = %d, want 41234", cfg.WebRTCUDPPort)
	}
	if !cfg.ForceRelayTransport {
		t.Error("force relay transport = false, want true")
	}
	if cfg.PortMappingEnabled {
		t.Error("port mapping enabled = true, want false")
	}
}

func TestLoadRejectsNonWebSocketGatewayURL(t *testing.T) {
	t.Setenv("HERDR_RELAY_TOKEN", "0123456789abcdef0123456789abcdef")
	t.Setenv("HERDR_GATEWAY_URL", "https://gw.example.com")

	if _, err := Load(); err == nil {
		t.Fatal("expected error for non-websocket gateway url")
	}
}

func TestLoadRejectsTokenlessGateway(t *testing.T) {
	t.Setenv("HERDR_RELAY_TOKEN", "")
	t.Setenv("HERDR_GATEWAY_URL", "wss://gw.example.com")

	if _, err := Load(); err == nil {
		t.Fatal("expected error for tokenless gateway registration")
	}
}

func TestLoadParsesOrderedGatewayList(t *testing.T) {
	t.Setenv("HERDR_RELAY_TOKEN", "0123456789abcdef0123456789abcdef")
	t.Setenv("HERDR_GATEWAY_URL", " wss://a.example.com , wss://b.example.com/ ,")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"wss://a.example.com", "wss://b.example.com"}
	if !slices.Equal(cfg.GatewayURLs, want) {
		t.Errorf("gateway urls = %v, want %v", cfg.GatewayURLs, want)
	}
	if cfg.GatewayURL != want[0] {
		t.Errorf("gateway url = %q, want the first list entry %q", cfg.GatewayURL, want[0])
	}
}

func TestLoadRejectsInvalidSecondGatewayURL(t *testing.T) {
	t.Setenv("HERDR_RELAY_TOKEN", "0123456789abcdef0123456789abcdef")
	t.Setenv("HERDR_GATEWAY_URL", "wss://a.example.com,https://b.example.com")

	_, err := Load()
	if err == nil {
		t.Fatal("expected an error for a non-websocket second gateway url")
	}
	if !strings.Contains(err.Error(), "https://b.example.com") {
		t.Errorf("error = %v, want the offending entry named", err)
	}
}
