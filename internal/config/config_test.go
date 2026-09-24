package config

import (
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	isolateLoadEnvironment(t)
	t.Setenv("HERDR_RELAY_LOG_LEVEL", "")

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
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("log level = %v, want %v", cfg.LogLevel, slog.LevelInfo)
	}
}

func TestParseLogLevel(t *testing.T) {
	for _, test := range []struct {
		name  string
		raw   string
		want  slog.Level
		error bool
	}{
		{name: "empty", raw: "", want: slog.LevelInfo},
		{name: "whitespace", raw: "  \t", want: slog.LevelInfo},
		{name: "debug", raw: "debug", want: slog.LevelDebug},
		{name: "info", raw: "info", want: slog.LevelInfo},
		{name: "warn", raw: "warn", want: slog.LevelWarn},
		{name: "error", raw: "error", want: slog.LevelError},
		{name: "mixed case and whitespace", raw: "  WaRn  ", want: slog.LevelWarn},
		{name: "verbose", raw: "verbose", error: true},
		{name: "warning", raw: "warning", error: true},
		{name: "off", raw: "off", error: true},
		{name: "numeric", raw: "7", error: true},
		{name: "level expression", raw: "INFO+1", error: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseLogLevel(test.raw)
			if test.error {
				if err == nil || !strings.Contains(err.Error(), "HERDR_RELAY_LOG_LEVEL") {
					t.Fatalf("parseLogLevel(%q) error = %v", test.raw, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseLogLevel(%q) error = %v", test.raw, err)
			}
			if got != test.want {
				t.Errorf("parseLogLevel(%q) = %v, want %v", test.raw, got, test.want)
			}
		})
	}
}

func TestLoadRejectsInvalidLogLevel(t *testing.T) {
	isolateLoadEnvironment(t)
	t.Setenv("HERDR_RELAY_LOG_LEVEL", "verbose")

	cfg, err := Load()
	if err == nil {
		t.Fatal("expected invalid log-level error")
	}
	if cfg != nil {
		t.Fatalf("config = %#v, want nil", cfg)
	}
	if !strings.Contains(err.Error(), "HERDR_RELAY_LOG_LEVEL") {
		t.Errorf("error = %v, want setting name", err)
	}
}

func TestLoadRejectsTokenlessNonLoopback(t *testing.T) {
	isolateLoadEnvironment(t)
	t.Setenv("HERDR_RELAY_HOST", "0.0.0.0")
	t.Setenv("HERDR_RELAY_TOKEN", "")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for tokenless non-loopback bind")
	}
}

func TestLoadRejectsShortRelayKey(t *testing.T) {
	isolateLoadEnvironment(t)
	t.Setenv("HERDR_RELAY_TOKEN", "predictable")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for relay key shorter than 16 bytes")
	}
}

func TestLoadAllowedOrigins(t *testing.T) {
	isolateLoadEnvironment(t)
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
	isolateLoadEnvironment(t)
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
	isolateLoadEnvironment(t)
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
	isolateLoadEnvironment(t)
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
	isolateLoadEnvironment(t)
	t.Setenv("HERDR_RELAY_TOKEN", "0123456789abcdef0123456789abcdef")
	t.Setenv("HERDR_GATEWAY_URL", "https://gw.example.com")

	if _, err := Load(); err == nil {
		t.Fatal("expected error for non-websocket gateway url")
	}
}

func TestLoadTailscaleRequiresSafeLoopbackAndNoRearm(t *testing.T) {
	isolateLoadEnvironment(t)
	t.Setenv("HERDR_RELAY_TOKEN", "0123456789abcdef0123456789abcdef")
	t.Setenv("HERDR_RELAY_TRANSPORT", "tailscale")
	t.Setenv("HERDR_TAILSCALE_ORIGIN", "relay.tailnet.ts.net")
	t.Setenv("HERDR_RELAY_INSTANCE_ID", "instance-1")
	t.Setenv("HERDR_RELAY_PAIRING_SOCKET", filepath.Join(t.TempDir(), "control.sock"))
	t.Setenv("HERDR_RELAY_RUN_ID", "run-1")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Transport != TransportTailscale || cfg.TailscaleOrigin != "https://relay.tailnet.ts.net" {
		t.Fatalf("tailscale config = %#v", cfg)
	}

	t.Setenv("HERDR_RELAY_REARM_BOOTSTRAP", "1")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "REARM_BOOTSTRAP") {
		t.Fatalf("rearm accepted: %v", err)
	}
}

func TestLoadRejectsTailscaleGatewayConflict(t *testing.T) {
	isolateLoadEnvironment(t)
	t.Setenv("HERDR_RELAY_TOKEN", "0123456789abcdef0123456789abcdef")
	t.Setenv("HERDR_RELAY_TRANSPORT", "tailscale")
	t.Setenv("HERDR_TAILSCALE_ORIGIN", "https://relay.tailnet.ts.net")
	t.Setenv("HERDR_RELAY_INSTANCE_ID", "instance-1")
	t.Setenv("HERDR_RELAY_PAIRING_SOCKET", filepath.Join(t.TempDir(), "control.sock"))
	t.Setenv("HERDR_RELAY_RUN_ID", "run-1")
	t.Setenv("HERDR_GATEWAY_URL", "wss://gateway.example.com")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("gateway conflict accepted: %v", err)
	}
}

func TestLoadRejectsTokenlessGateway(t *testing.T) {
	isolateLoadEnvironment(t)
	t.Setenv("HERDR_RELAY_TOKEN", "")
	t.Setenv("HERDR_GATEWAY_URL", "wss://gw.example.com")

	if _, err := Load(); err == nil {
		t.Fatal("expected error for tokenless gateway registration")
	}
}

func TestLoadParsesOrderedGatewayList(t *testing.T) {
	isolateLoadEnvironment(t)
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

// TestLoadNormalisesGatewaySelection pins the rule the gateway transport reads
// without re-validating it. Only an explicit "latency" opts into RTT ranking,
// so a typo or an env file from a newer release still honours the configured
// order instead of silently ranking a hand-listed gateway away.
func TestLoadNormalisesGatewaySelection(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  string
	}{
		{name: "empty", want: GatewaySelectionOrdered},
		{name: "ordered", value: "ordered", want: GatewaySelectionOrdered},
		{name: "upper case latency", value: " LATENCY ", want: GatewaySelectionLatency},
		{name: "unrecognised", value: "fastest-wins", want: GatewaySelectionOrdered},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateLoadEnvironment(t)
			t.Setenv("HERDR_RELAY_TOKEN", "0123456789abcdef0123456789abcdef")
			t.Setenv("HERDR_GATEWAY_URL", "wss://mine.example.com,wss://community.example.com")
			t.Setenv("HERDR_GATEWAY_SELECTION", tc.value)
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.GatewaySelection != tc.want {
				t.Errorf("gateway selection = %q, want %q", cfg.GatewaySelection, tc.want)
			}
		})
	}
}

func TestLoadRejectsInvalidSecondGatewayURL(t *testing.T) {
	isolateLoadEnvironment(t)
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

func TestLoadTailscaleKeyAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, token string
		valid       bool
	}{
		{name: "empty"},
		{name: "short", token: strings.Repeat("x", 31)},
		{name: "long", token: strings.Repeat("x", 33)},
		{name: "exact nonhex", token: strings.Repeat("z", 32), valid: true},
		{name: "exact multibyte", token: strings.Repeat("é", 16), valid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateTailscaleEnvironment(t)
			t.Setenv("HERDR_RELAY_TOKEN", tc.token)
			cfg, err := Load()
			if !tc.valid {
				if cfg != nil || err == nil || !strings.Contains(err.Error(), "32 bytes") {
					t.Fatalf("want nil config and 32-byte key refusal; config present=%t, error=%v", cfg != nil, err)
				}
				return
			}
			if err != nil || cfg == nil {
				t.Fatalf("valid key refused: %v", err)
			}
			if cfg.Token != tc.token {
				t.Fatal("key representation changed")
			}
		})
	}
}

func TestLoadRearmAdmission(t *testing.T) {
	for _, transport := range []string{TransportTailscale, TransportCloudflare} {
		for _, raw := range []string{"1", "t", "T", "TRUE", "True", "true", "0", "f", "F", "FALSE", "False", "false", ""} {
			t.Run(transport+"/"+raw, func(t *testing.T) {
				isolateTailscaleEnvironment(t)
				t.Setenv("HERDR_RELAY_TRANSPORT", transport)
				t.Setenv("HERDR_RELAY_REARM_BOOTSTRAP", raw)
				wantRearm := raw == "1" || raw == "t" || raw == "T" || raw == "TRUE" || raw == "True" || raw == "true"
				cfg, err := Load()
				if transport == TransportTailscale && wantRearm {
					if cfg != nil || err == nil || !strings.Contains(err.Error(), "REARM_BOOTSTRAP") {
						t.Fatalf("want nil config and rearm refusal; config present=%t, error=%v", cfg != nil, err)
					}
					return
				}
				if err != nil || cfg == nil {
					t.Fatalf("load failed: %v", err)
				}
				if cfg.RearmBootstrap != wantRearm {
					t.Fatalf("rearm = %t, want %t", cfg.RearmBootstrap, wantRearm)
				}
			})
		}
	}
}

func TestValidateTailscaleRejectsParsedRearm(t *testing.T) {
	for _, raw := range []string{"", "false"} {
		t.Run(raw, func(t *testing.T) {
			isolateLoadEnvironment(t)
			t.Setenv("HERDR_RELAY_REARM_BOOTSTRAP", raw)
			cfg := Config{
				Transport: TransportTailscale, Host: "127.0.0.1", Port: 8375, PluginPort: 8376,
				Token: strings.Repeat("z", 32), TailscaleOrigin: "https://relay.tailnet.ts.net",
				InstanceID: "instance-1", ManagedRunID: "run-1",
				PairingSocketPath: filepath.Join(t.TempDir(), "control.sock"),
			}
			if err := cfg.validate(); err != nil {
				t.Fatalf("valid control refused: %v", err)
			}
			cfg.RearmBootstrap = true
			if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "REARM_BOOTSTRAP") {
				t.Fatalf("parsed rearm accepted: %v", err)
			}
		})
	}
}

func TestLoadLegacyTokenlessAdmission(t *testing.T) {
	for _, transport := range []string{"", TransportCloudflare, TransportGateway} {
		for _, host := range []string{"127.0.0.1", "::1", "localhost", "0.0.0.0"} {
			t.Run(transport+"/"+host, func(t *testing.T) {
				isolateLoadEnvironment(t)
				t.Setenv("HERDR_RELAY_TRANSPORT", transport)
				t.Setenv("HERDR_RELAY_HOST", host)
				if transport == TransportGateway {
					t.Setenv("HERDR_GATEWAY_URL", "wss://gw.example.com")
				}
				cfg, err := Load()
				wantError := ""
				if host == "0.0.0.0" {
					wantError = "non-loopback"
				} else if transport == TransportGateway {
					wantError = "requires a relay key"
				}
				if wantError != "" {
					if cfg != nil || err == nil || !strings.Contains(err.Error(), wantError) {
						t.Fatalf("want nil config and %s refusal; config present=%t, error=%v", wantError, cfg != nil, err)
					}
					return
				}
				if err != nil || cfg == nil {
					t.Fatalf("legacy loopback refused: %v", err)
				}
			})
		}
	}
}

func isolateTailscaleEnvironment(t *testing.T) {
	t.Helper()
	isolateLoadEnvironment(t)
	t.Setenv("HERDR_RELAY_TOKEN", strings.Repeat("z", 32))
	t.Setenv("HERDR_RELAY_TRANSPORT", TransportTailscale)
	t.Setenv("HERDR_TAILSCALE_ORIGIN", "https://relay.tailnet.ts.net")
	t.Setenv("HERDR_RELAY_INSTANCE_ID", "instance-1")
	t.Setenv("HERDR_RELAY_PAIRING_SOCKET", filepath.Join(t.TempDir(), "control.sock"))
	t.Setenv("HERDR_RELAY_RUN_ID", "run-1")
}

func isolateLoadEnvironment(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HERDR_RELAY_HOST", "127.0.0.1")
	t.Setenv("HERDR_RELAY_PORT", "")
	t.Setenv("HERDR_RELAY_PLUGIN_PORT", "")
	t.Setenv("HERDR_RELAY_TOKEN", "")
	t.Setenv("HERDR_RELAY_INSTANCE_ID", "")
	t.Setenv("HERDR_WEB_ROOT", "")
	t.Setenv("HERDR_BIN", "/bin/false")
	t.Setenv("HERDR_SOCKET_PATH", "")
	t.Setenv("HERDR_RELAY_POLL_INTERVAL", "")
	t.Setenv("HERDR_RELAY_LOG_FORMAT", "")
	t.Setenv("HERDR_RELAY_LOG_LEVEL", "")
	t.Setenv("HERDR_RELAY_SERVICE_NAME", "")
	t.Setenv("HERDR_ALLOWED_ORIGINS", "")
	t.Setenv("HERDR_GATEWAY_URL", "")
	t.Setenv("HERDR_GATEWAY_SELECTION", "")
	t.Setenv("HERDR_WEBRTC_UDP_PORT", "")
	t.Setenv("HERDR_TRANSPORT_FORCE_RELAY", "")
	t.Setenv("HERDR_REACHABILITY_PORT_MAPPING", "")
	t.Setenv("HERDR_RELAY_REARM_BOOTSTRAP", "")
	t.Setenv("HERDR_RELAY_TRANSPORT", "")
	t.Setenv("HERDR_TAILSCALE_ORIGIN", "")
	t.Setenv("HERDR_RELAY_PAIRING_SOCKET", "")
	t.Setenv("HERDR_RELAY_RUN_ID", "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("HERDR_RELAY_ENV", "")
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", "")
	t.Setenv("HERDR_RELEASE_ROOT", "")
}
