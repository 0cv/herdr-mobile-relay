package release

import (
	"errors"
	"fmt"
	"strings"

	relayprotocol "github.com/0cv/herdr-mobile-relay/internal/protocol"
)

const maxTransportCapabilities = 8

// TransportCapabilities returns the transports supported by an installed
// release. Releases created before this metadata existed spoke E2EE v1 only.
func TransportCapabilities(manifest Manifest) (app, relay []string) {
	app = manifest.AppTransports
	if len(app) == 0 {
		app = []string{relayprotocol.EncryptedWebSocketSubprotocol}
	}
	relay = manifest.RelayTransports
	if len(relay) == 0 {
		relay = []string{relayprotocol.EncryptedWebSocketSubprotocol}
	}
	return app, relay
}

// ValidateUpgradeCompatibility requires both app-first and relay-first rollout
// windows to remain connected. An incompatible transport change therefore
// needs an intermediate bridge release that supports both transports.
func ValidateUpgradeCompatibility(current, target Manifest) error {
	if len(target.AppTransports) == 0 || len(target.RelayTransports) == 0 {
		return errors.New("target release does not declare app/relay transport compatibility")
	}
	if err := validateTransportCapabilities("target app", target.AppTransports); err != nil {
		return err
	}
	if err := validateTransportCapabilities("target relay", target.RelayTransports); err != nil {
		return err
	}

	currentApp, currentRelay := TransportCapabilities(current)
	if err := validateTransportCapabilities("current app", currentApp); err != nil {
		return err
	}
	if err := validateTransportCapabilities("current relay", currentRelay); err != nil {
		return err
	}
	if !transportsIntersect(target.AppTransports, currentRelay) {
		return errors.New("target app cannot connect to the current relay; install a bridge release first")
	}
	if !transportsIntersect(currentApp, target.RelayTransports) {
		return errors.New("current app cannot connect to the target relay; install a bridge release first")
	}
	return nil
}

func validateTransportCapabilities(owner string, transports []string) error {
	if len(transports) > maxTransportCapabilities {
		return fmt.Errorf("%s declares too many transports", owner)
	}
	seen := make(map[string]bool, len(transports))
	for _, transport := range transports {
		if transport == "" || strings.TrimSpace(transport) != transport || len(transport) > 64 {
			return fmt.Errorf("%s declares invalid transport %q", owner, transport)
		}
		if seen[transport] {
			return fmt.Errorf("%s declares duplicate transport %q", owner, transport)
		}
		seen[transport] = true
	}
	return nil
}

func transportsIntersect(left, right []string) bool {
	for _, candidate := range left {
		for _, available := range right {
			if candidate == available {
				return true
			}
		}
	}
	return false
}
