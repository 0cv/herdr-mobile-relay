package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/0cv/herdr-mobile-relay/internal/setuphelper"
)

// Accepted relay transport values.
const (
	TransportCloudflare        = "cloudflare"
	TransportGateway           = "gateway"
	TransportTailscale         = "tailscale"
	TransportTailscaleExternal = "tailscale-external"

	// Accepted HERDR_GATEWAY_SELECTION values.
	// GatewaySelectionOrdered registers with the first healthy entry in
	// configured order: an explicit list is a priority, not a preference.
	GatewaySelectionOrdered = "ordered"
	// GatewaySelectionLatency ranks healthy entries by measured round trip. It
	// only fits interchangeable endpoints, such as the community gateway list.
	GatewaySelectionLatency = "latency"
)

type Config struct {
	Host                string
	Port                int
	PluginPort          int
	Token               string
	InstanceID          string
	AllowedOrigins      []string
	WebRoot             string
	HerdrBin            string
	SocketPath          string
	PollInterval        float64
	RuntimeDir          string
	LogFormat           string
	LogLevel            slog.Level
	ReleaseRoot         string
	ServiceName         string
	Transport           string
	TailscaleOrigin     string
	ExternalHTTPSOrigin string
	PhoneAppOrigin      string
	TailscaleBin        string
	PairingSocketPath   string
	ManagedRunID        string
	ControlRunID        string

	// GatewayURL is the configured tie-break leader, kept equal to
	// GatewayURLs[0] so readers that only know one gateway keep working. The
	// transport may select another healthy entry at runtime.
	GatewayURL  string
	GatewayURLs []string
	// GatewaySelection is how the transport picks among GatewayURLs: "ordered"
	// registers with the first healthy entry in configured order, "latency"
	// with the lowest-latency healthy one. The loader normalises it, so no
	// reader validates it again.
	GatewaySelection    string
	WebRTCUDPPort       int
	ForceRelayTransport bool
	PortMappingEnabled  bool
	// RearmBootstrap starts every process with an empty device list and a fresh
	// one-use bootstrap invitation. Only the quick-tunnel flow sets it: its app
	// origin changes each launch, so no enrolled credential can be presented again.
	RearmBootstrap bool

	CacheDir   string
	ConfigHome string
	DataHome   string
}

func Load() (*Config, error) {
	transport, err := resolveTransport(os.Getenv("HERDR_RELAY_TRANSPORT"), os.Getenv("HERDR_GATEWAY_URL"))
	if err != nil {
		return nil, err
	}
	cfg := &Config{
		Host:                envOr("HERDR_RELAY_HOST", "127.0.0.1"),
		Port:                envIntOr("HERDR_RELAY_PORT", 8375),
		PluginPort:          envIntOr("HERDR_RELAY_PLUGIN_PORT", 8376),
		Token:               os.Getenv("HERDR_RELAY_TOKEN"),
		InstanceID:          os.Getenv("HERDR_RELAY_INSTANCE_ID"),
		WebRoot:             os.Getenv("HERDR_WEB_ROOT"),
		HerdrBin:            os.Getenv("HERDR_BIN"),
		SocketPath:          os.Getenv("HERDR_SOCKET_PATH"),
		PollInterval:        envFloatOr("HERDR_RELAY_POLL_INTERVAL", 2.0),
		LogFormat:           envOr("HERDR_RELAY_LOG_FORMAT", "text"),
		ServiceName:         envOr("HERDR_RELAY_SERVICE_NAME", defaultServiceName()),
		Transport:           transport,
		TailscaleOrigin:     os.Getenv("HERDR_TAILSCALE_ORIGIN"),
		ExternalHTTPSOrigin: os.Getenv("HERDR_EXTERNAL_HTTPS_ORIGIN"),
		PhoneAppOrigin:      os.Getenv("HERDR_PHONE_APP_URL"),
		TailscaleBin:        envOr("HERDR_TAILSCALE_BIN", "tailscale"),
		PairingSocketPath:   os.Getenv("HERDR_RELAY_PAIRING_SOCKET"),
		ManagedRunID:        os.Getenv("HERDR_RELAY_RUN_ID"),
		ControlRunID:        os.Getenv("HERDR_RELAY_CONTROL_RUN_ID"),

		WebRTCUDPPort:       envIntOr("HERDR_WEBRTC_UDP_PORT", 0),
		ForceRelayTransport: envBoolOr("HERDR_TRANSPORT_FORCE_RELAY", false),
		PortMappingEnabled:  envBoolOr("HERDR_REACHABILITY_PORT_MAPPING", true),
		RearmBootstrap:      envBoolOr("HERDR_RELAY_REARM_BOOTSTRAP", false),
	}

	logLevel, err := parseLogLevel(os.Getenv("HERDR_RELAY_LOG_LEVEL"))
	if err != nil {
		return nil, err
	}
	cfg.LogLevel = logLevel

	if origins := os.Getenv("HERDR_ALLOWED_ORIGINS"); origins != "" {
		for _, o := range strings.Split(origins, ",") {
			if trimmed := strings.TrimSpace(o); trimmed != "" {
				cfg.AllowedOrigins = append(cfg.AllowedOrigins, trimmed)
			}
		}
	}

	// HERDR_GATEWAY_URL is an ordered candidate list. The relay probes the
	// entries concurrently; HERDR_GATEWAY_SELECTION decides what the order
	// means. A single value is one entry and behaves exactly as it always did.
	cfg.GatewayURLs = parseGatewayURLs(os.Getenv("HERDR_GATEWAY_URL"))
	if len(cfg.GatewayURLs) > 0 {
		cfg.GatewayURL = cfg.GatewayURLs[0]
	}
	cfg.GatewaySelection = parseGatewaySelection(os.Getenv("HERDR_GATEWAY_SELECTION"))

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
	if c.Transport == "" {
		c.Transport = inferredTransport(c.GatewayURLs)
	}
	if c.Transport != TransportCloudflare && c.Transport != TransportGateway &&
		c.Transport != TransportTailscale && c.Transport != TransportTailscaleExternal {
		return fmt.Errorf("invalid HERDR_RELAY_TRANSPORT %q", c.Transport)
	}
	if c.Token == "" && c.Host != "127.0.0.1" && c.Host != "::1" && c.Host != "localhost" {
		return fmt.Errorf("refusing to bind tokenless relay to non-loopback address %s", c.Host)
	}
	if c.Token != "" && len(c.Token) != 32 {
		return errors.New("relay key must be exactly 32 bytes")
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("invalid relay port %d", c.Port)
	}
	if c.PluginPort < 1 || c.PluginPort > 65535 {
		return fmt.Errorf("invalid plugin port %d", c.PluginPort)
	}
	if c.Transport == TransportGateway && len(c.GatewayURLs) == 0 {
		return errors.New("gateway transport requires HERDR_GATEWAY_URL")
	}
	if c.Transport != TransportGateway && len(c.GatewayURLs) > 0 {
		return fmt.Errorf("HERDR_GATEWAY_URL conflicts with %s transport", c.Transport)
	}
	for _, gateway := range c.GatewayURLs {
		parsed, err := url.Parse(gateway)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "ws" && parsed.Scheme != "wss") {
			return fmt.Errorf("invalid gateway url %q: want ws:// or wss:// base url", gateway)
		}
	}
	if len(c.GatewayURLs) > 0 && c.Token == "" {
		return fmt.Errorf("gateway url requires a relay key: the gateway path derives its credentials from it")
	}
	if c.Transport == TransportTailscaleExternal {
		return c.validateExternalTailscale()
	}
	if c.Transport != TransportTailscale {
		return nil
	}
	if c.Token == "" {
		return errors.New("tailscale transport requires a relay key of exactly 32 bytes")
	}
	if c.Host != "127.0.0.1" {
		return fmt.Errorf("tailscale transport requires HERDR_RELAY_HOST=127.0.0.1, got %q", c.Host)
	}
	if strings.TrimSpace(os.Getenv("HERDR_GATEWAY_SELECTION")) != "" {
		return errors.New("HERDR_GATEWAY_SELECTION conflicts with tailscale transport")
	}
	if c.RearmBootstrap {
		return errors.New("tailscale transport refuses HERDR_RELAY_REARM_BOOTSTRAP; it would reset device credentials")
	}
	if c.TailscaleOrigin == "" {
		return errors.New("tailscale transport requires a verified HERDR_TAILSCALE_ORIGIN")
	}
	origin, err := setuphelper.NormalizeOrigin(c.TailscaleOrigin, false)
	if err != nil || !strings.HasPrefix(origin, "https://") {
		return errors.New("HERDR_TAILSCALE_ORIGIN must be an HTTPS origin without a path")
	}
	c.TailscaleOrigin = origin
	if c.InstanceID == "" || !safeRunID(c.InstanceID) {
		return errors.New("tailscale transport requires a valid relay instance ID")
	}
	if c.PairingSocketPath == "" || c.ManagedRunID == "" {
		return errors.New("managed tailscale startup requires both pairing socket and run id")
	}
	if !filepath.IsAbs(c.PairingSocketPath) {
		return errors.New("HERDR_RELAY_PAIRING_SOCKET must be an absolute path")
	}
	if !safeRunID(c.ManagedRunID) {
		return errors.New("HERDR_RELAY_RUN_ID is invalid")
	}
	return nil
}

func (c *Config) validateExternalTailscale() error {
	if c.Token == "" {
		return errors.New("tailscale-external transport requires a relay key of exactly 32 bytes")
	}
	if c.Host != "127.0.0.1" {
		return fmt.Errorf("tailscale-external transport requires HERDR_RELAY_HOST=127.0.0.1, got %q", c.Host)
	}
	if strings.TrimSpace(os.Getenv("HERDR_GATEWAY_SELECTION")) != "" {
		return errors.New("HERDR_GATEWAY_SELECTION conflicts with tailscale-external transport")
	}
	if c.RearmBootstrap {
		return errors.New("tailscale-external transport refuses HERDR_RELAY_REARM_BOOTSTRAP; it would reset device credentials")
	}
	if c.PortMappingEnabled {
		return errors.New("tailscale-external transport refuses automatic PCP/UPnP port mapping; set HERDR_REACHABILITY_PORT_MAPPING=0")
	}
	if c.ManagedRunID != "" {
		return errors.New("tailscale-external transport must not use managed Tailscale run ownership")
	}
	if c.TailscaleOrigin != "" {
		return errors.New("HERDR_TAILSCALE_ORIGIN conflicts with tailscale-external transport")
	}
	origin, err := setuphelper.NormalizeExternalHTTPSOrigin(c.ExternalHTTPSOrigin)
	if err != nil || origin != c.ExternalHTTPSOrigin {
		return errors.New("HERDR_EXTERNAL_HTTPS_ORIGIN must be a canonical HTTPS origin")
	}
	phoneAppOrigin, err := setuphelper.NormalizeExternalHTTPSOrigin(c.PhoneAppOrigin)
	if err != nil || phoneAppOrigin != c.PhoneAppOrigin {
		return errors.New("HERDR_PHONE_APP_URL must be a canonical HTTPS origin for external Serve")
	}
	if c.InstanceID == "" || !safeRunID(c.InstanceID) {
		return errors.New("tailscale-external transport requires a valid relay instance ID")
	}
	if c.ControlRunID == "" || !safeRunID(c.ControlRunID) {
		return errors.New("tailscale-external transport requires a valid private control run ID")
	}
	if c.PairingSocketPath == "" || !filepath.IsAbs(c.PairingSocketPath) {
		return errors.New("tailscale-external transport requires an absolute HERDR_RELAY_PAIRING_SOCKET")
	}
	return nil
}

func resolveTransport(raw, gatewayRaw string) (string, error) {
	transport := strings.ToLower(strings.TrimSpace(raw))
	gatewayConfigured := len(parseGatewayURLs(gatewayRaw)) > 0
	if transport == "" {
		if gatewayConfigured {
			return TransportGateway, nil
		}
		return TransportCloudflare, nil
	}
	switch transport {
	case TransportCloudflare:
		if gatewayConfigured {
			return "", errors.New("HERDR_RELAY_TRANSPORT=cloudflare conflicts with HERDR_GATEWAY_URL")
		}
	case TransportGateway:
		if !gatewayConfigured {
			return "", errors.New("HERDR_RELAY_TRANSPORT=gateway requires HERDR_GATEWAY_URL")
		}
	case TransportTailscale:
		if gatewayConfigured {
			return "", errors.New("HERDR_RELAY_TRANSPORT=tailscale conflicts with HERDR_GATEWAY_URL")
		}
	case TransportTailscaleExternal:
		if gatewayConfigured {
			return "", errors.New("HERDR_RELAY_TRANSPORT=tailscale-external conflicts with HERDR_GATEWAY_URL")
		}
	default:
		return "", fmt.Errorf("invalid HERDR_RELAY_TRANSPORT %q", raw)
	}
	return transport, nil
}

func inferredTransport(gateways []string) string {
	if len(gateways) > 0 {
		return TransportGateway
	}
	return TransportCloudflare
}

func truthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func safeRunID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if character < 0x21 || character > 0x7e || character == '/' || character == '\\' {
			return false
		}
	}
	return true
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
	if webRoot := extractedReleaseWebRoot(exe); webRoot != "" {
		return webRoot
	}
	return filepath.Join(filepath.Dir(filepath.Dir(exe)), "web")
}

func extractedReleaseWebRoot(exe string) string {
	releaseDir := filepath.Dir(exe)
	manifest, err := os.Lstat(filepath.Join(releaseDir, "release-manifest.json"))
	if err != nil || !manifest.Mode().IsRegular() {
		return ""
	}
	webRoot := filepath.Join(releaseDir, "web")
	webInfo, err := os.Lstat(webRoot)
	if err != nil || !webInfo.IsDir() {
		return ""
	}
	descriptor, err := os.Lstat(filepath.Join(webRoot, "release.json"))
	if err != nil || !descriptor.Mode().IsRegular() {
		return ""
	}
	return webRoot
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

// parseGatewayURLs splits the ordered gateway list. Empty entries are dropped
// so a trailing comma or a stray space in a hand-edited env file configures a
// working relay instead of a phantom gateway.
func parseGatewayURLs(raw string) []string {
	var urls []string
	for _, entry := range strings.Split(raw, ",") {
		if trimmed := strings.TrimRight(strings.TrimSpace(entry), "/"); trimmed != "" {
			urls = append(urls, trimmed)
		}
	}
	return urls
}

// parseGatewaySelection normalises the selection rule. Only the community
// gateway list is a set of interchangeable endpoints where latency ranking is
// the point; a hand-listed gateway is a choice the relay must honour, so
// absent, empty and unrecognised values all mean configured order.
func parseGatewaySelection(raw string) string {
	if strings.ToLower(strings.TrimSpace(raw)) == GatewaySelectionLatency {
		return GatewaySelectionLatency
	}
	return GatewaySelectionOrdered
}

func parseLogLevel(raw string) (slog.Level, error) {
	switch normalized := strings.ToLower(strings.TrimSpace(raw)); normalized {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("invalid HERDR_RELAY_LOG_LEVEL %q: want debug, info, warn, or error", raw)
	}
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

func envBoolOr(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
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
