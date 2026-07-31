package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

type Config struct {
	Host           string
	Port           int
	PluginPort     int
	Token          string
	InstanceID     string
	AllowedOrigins []string
	WebRoot        string
	HerdrBin       string
	SocketPath     string
	PollInterval   float64
	RuntimeDir     string
	LogFormat      string
	ReleaseRoot    string
	ServiceName    string

	CacheDir   string
	ConfigHome string
	DataHome   string
}

func Load() (*Config, error) {
	cfg := &Config{
		Host:         envOr("HERDR_RELAY_HOST", "127.0.0.1"),
		Port:         envIntOr("HERDR_RELAY_PORT", 8375),
		PluginPort:   envIntOr("HERDR_RELAY_PLUGIN_PORT", 8376),
		Token:        os.Getenv("HERDR_RELAY_TOKEN"),
		InstanceID:   os.Getenv("HERDR_RELAY_INSTANCE_ID"),
		WebRoot:      os.Getenv("HERDR_WEB_ROOT"),
		HerdrBin:     os.Getenv("HERDR_BIN"),
		SocketPath:   os.Getenv("HERDR_SOCKET_PATH"),
		PollInterval: envFloatOr("HERDR_RELAY_POLL_INTERVAL", 2.0),
		LogFormat:    envOr("HERDR_RELAY_LOG_FORMAT", "text"),
		ServiceName:  envOr("HERDR_RELAY_SERVICE_NAME", defaultServiceName()),
	}

	if origins := os.Getenv("HERDR_ALLOWED_ORIGINS"); origins != "" {
		for _, o := range strings.Split(origins, ",") {
			if trimmed := strings.TrimSpace(o); trimmed != "" {
				cfg.AllowedOrigins = append(cfg.AllowedOrigins, trimmed)
			}
		}
	}

	cfg.ConfigHome = envOr("XDG_CONFIG_HOME", filepath.Join(homeDir(), ".config"))
	cacheHome := envOr("XDG_CACHE_HOME", filepath.Join(homeDir(), ".cache"))
	cfg.DataHome = envOr("XDG_DATA_HOME", filepath.Join(homeDir(), ".local", "share"))
	cfg.ReleaseRoot = os.Getenv("HERDR_RELEASE_ROOT")
	if cfg.ReleaseRoot == "" {
		cfg.ReleaseRoot = installedReleaseRoot()
	}
	if cfg.ReleaseRoot == "" {
		cfg.ReleaseRoot = filepath.Join(cfg.DataHome, "herdr-mobile-relay")
	}

	if cfg.SocketPath == "" {
		cfg.SocketPath = filepath.Join(cfg.ConfigHome, "herdr", "herdr.sock")
	}

	cfg.RuntimeDir = resolveRuntimeDir(cfg.ConfigHome)
	cfg.CacheDir = filepath.Join(cacheHome, "herdr-mobile-relay")

	if cfg.WebRoot == "" {
		cfg.WebRoot = defaultWebRoot()
	}

	if cfg.HerdrBin == "" {
		cfg.HerdrBin = findHerdrBin()
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

func (c *Config) Addr() string {
	return net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
}

func (c *Config) validate() error {
	if c.Token == "" && c.Host != "127.0.0.1" && c.Host != "::1" && c.Host != "localhost" {
		return fmt.Errorf("refusing to bind tokenless relay to non-loopback address %s", c.Host)
	}
	if c.Token != "" && len(c.Token) < 16 {
		return fmt.Errorf("relay key must be at least 16 bytes")
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("invalid port %d", c.Port)
	}
	return nil
}

func resolveRuntimeDir(configHome string) string {
	if env := os.Getenv("HERDR_RELAY_ENV"); env != "" {
		return filepath.Dir(env)
	}
	if dir := os.Getenv("HERDR_PLUGIN_CONFIG_DIR"); dir != "" {
		return dir
	}
	return filepath.Join(configHome, "herdr-mobile-relay")
}

func defaultWebRoot() string {
	exe, err := os.Executable()
	if err != nil {
		return "web"
	}
	if root := installedReleaseRoot(); root != "" {
		if resolved, resolveErr := filepath.EvalSymlinks(exe); resolveErr == nil {
			return filepath.Join(filepath.Dir(resolved), "web")
		}
		return filepath.Join(filepath.Dir(exe), "web")
	}
	return filepath.Join(filepath.Dir(filepath.Dir(exe)), "web")
}

func installedReleaseRoot() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		resolved = exe
	}
	releaseDir := filepath.Dir(resolved)
	releasesDir := filepath.Dir(releaseDir)
	if filepath.Base(releasesDir) != "releases" {
		return ""
	}
	root := filepath.Dir(releasesDir)
	current := filepath.Join(root, "current")
	if _, err := os.Lstat(current); err != nil {
		return ""
	}
	return root
}

func defaultServiceName() string {
	if runtime.GOOS == "darwin" {
		return "com.herdr-mobile-relay.service"
	}
	return "herdr-mobile-relay.service"
}

func findHerdrBin() string {
	candidates := []string{
		filepath.Join(homeDir(), ".local", "bin", "herdr"),
		"/opt/homebrew/bin/herdr",
		"/usr/local/bin/herdr",
		"/home/linuxbrew/.linuxbrew/bin/herdr",
		"/home/linuxbrew/.linuxbrew/opt/herdr/bin/herdr",
	}
	if p, err := lookPath("herdr"); err == nil {
		return p
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c
		}
	}
	return "herdr"
}

func lookPath(name string) (string, error) {
	pathEnv := os.Getenv("PATH")
	for _, dir := range filepath.SplitList(pathEnv) {
		p := filepath.Join(dir, name)
		if info, err := os.Stat(p); err == nil && !info.IsDir() && info.Mode()&0111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s not found in PATH", name)
}

func homeDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "/tmp"
	}
	return h
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envIntOr(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func envFloatOr(key string, fallback float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return fallback
}
