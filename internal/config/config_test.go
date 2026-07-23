package config

import (
	"os"
	"path/filepath"
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
