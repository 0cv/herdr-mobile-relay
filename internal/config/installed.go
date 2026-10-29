package config

import (
	"net/url"
	"reflect"
	"strconv"

	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

// InstalledConfig retains a private deep snapshot, including secrets, which is
// never reportable. It is configuration validation, not profile enablement.
type InstalledConfig struct {
	snapshot Config
	binding  tailscalecli.InstalledBinding
}

func (c InstalledConfig) String() string   { return "installed configuration (redacted)" }
func (c InstalledConfig) GoString() string { return c.String() }

func cloneInstalledConfig(c Config) Config {
	if c.AllowedOrigins != nil {
		c.AllowedOrigins = append([]string{}, c.AllowedOrigins...)
	}
	if c.GatewayURLs != nil {
		c.GatewayURLs = append([]string{}, c.GatewayURLs...)
	}
	return c
}

// BindInstalledCLI validates only selected data: it never rereads environment,
// creates directories, enables a profile or invokes the CLI.
func BindInstalledCLI(cfg *Config) (InstalledConfig, error) {
	if cfg == nil {
		return InstalledConfig{}, tailscalecli.ErrWorkflowRequired
	}
	c := cloneInstalledConfig(*cfg)
	if c.Transport != TransportTailscaleCLI || c.Host != "127.0.0.1" || c.ManagedRunID != "" || c.ControlRunID != "" || c.PairingSocketPath != "" || c.RearmBootstrap || c.PortMappingEnabled || c.GatewayURL != "" || len(c.GatewayURLs) != 0 || (c.GatewaySelection != "" && c.GatewaySelection != GatewaySelectionOrdered) || c.TailscaleCLIDevelopmentRoot != "" || c.TailscaleOrigin != "" || c.ExternalHTTPSOrigin != "" {
		return InstalledConfig{}, tailscalecli.ErrWorkflowRequired
	}
	u, err := url.Parse(c.TailscaleCLIOrigin)
	if err != nil {
		return InstalledConfig{}, tailscalecli.ErrWorkflowRequired
	}
	port := 443
	if u.Port() != "" {
		port, err = strconv.Atoi(u.Port())
		if err != nil {
			return InstalledConfig{}, tailscalecli.ErrWorkflowRequired
		}
	}
	b, err := tailscalecli.NewInstalledBinding(tailscalecli.InstalledBindingInput{
		StateRoot: c.TailscaleCLIStateRoot, CoordinationRoot: c.TailscaleCLICoordinationRoot, Binary: c.TailscaleCLIBin,
		RuntimeRoot: c.RuntimeDir, CacheRoot: c.CacheDir, ConfigRoot: c.ConfigHome, ReleaseRoot: c.ReleaseRoot,
		Scope: c.TailscaleCLIScope, InstallationID: c.InstanceID, Origin: c.TailscaleCLIOrigin, PhoneAppOrigin: c.PhoneAppOrigin,
		HTTPSPort: port, BackendPort: c.Port, PluginPort: c.PluginPort,
	}, c.Token)
	if err != nil {
		return InstalledConfig{}, err
	}
	return InstalledConfig{snapshot: c, binding: b}, nil
}

func (c InstalledConfig) Snapshot() Config                       { return cloneInstalledConfig(c.snapshot) }
func (c InstalledConfig) Binding() tailscalecli.InstalledBinding { return c.binding }

// Validate checks the exact selected snapshot, not a newly read environment.
func (c InstalledConfig) Validate(cfg *Config) error {
	if cfg == nil || !reflect.DeepEqual(c.snapshot, cloneInstalledConfig(*cfg)) {
		return tailscalecli.ErrWorkflowRequired
	}
	return c.binding.Validate()
}
