package config

import (
	"net/url"
	"os"
	"strconv"

	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

// Reject raw policy typos before the shared permissive legacy parsers can
// substitute defaults. This is data validation, not activation authority.
func validateInstalledEnvironment(c *Config) error {
	switch os.Getenv("HERDR_RELAY_REARM_BOOTSTRAP") {
	case "", "0", "false", "False", "FALSE", "f", "F":
	default:
		return tailscalecli.ErrWorkflowRequired
	}
	if raw := os.Getenv("HERDR_REACHABILITY_PORT_MAPPING"); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil || v {
			return tailscalecli.ErrWorkflowRequired
		}
	}
	if raw := os.Getenv("HERDR_GATEWAY_SELECTION"); raw != "" && raw != GatewaySelectionOrdered {
		return tailscalecli.ErrWorkflowRequired
	}
	for _, key := range []string{"HERDR_RELAY_PORT", "HERDR_RELAY_PLUGIN_PORT"} {
		if raw := os.Getenv(key); raw != "" {
			if _, err := strconv.Atoi(raw); err != nil {
				return tailscalecli.ErrWorkflowRequired
			}
		}
	}
	if raw := os.Getenv("HERDR_TAILSCALE_CLI_HTTPS_PORT"); raw != "" {
		u, err := url.Parse(c.TailscaleCLIOrigin)
		if err != nil {
			return tailscalecli.ErrWorkflowRequired
		}
		port := u.Port()
		if port == "" {
			port = "443"
		}
		if raw != port {
			return tailscalecli.ErrWorkflowRequired
		}
	}
	return nil
}
