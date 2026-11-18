package release

import (
	"errors"
	"fmt"
	"strings"
)

const maxTransportCapabilities = 8

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

	if len(current.AppTransports) == 0 || len(current.RelayTransports) == 0 {
		return errors.New("current release does not declare app/relay transport compatibility")
	}
	if err := validateTransportCapabilities("current app", current.AppTransports); err != nil {
		return err
	}
	if err := validateTransportCapabilities("current relay", current.RelayTransports); err != nil {
		return err
	}
	if !transportsIntersect(target.AppTransports, current.RelayTransports) {
		return errors.New("target app cannot connect to the current relay; install a bridge release first")
	}
	if !transportsIntersect(current.AppTransports, target.RelayTransports) {
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
