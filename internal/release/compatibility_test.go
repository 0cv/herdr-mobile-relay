package release

import (
	"strings"
	"testing"

	relayprotocol "github.com/0cv/herdr-mobile-relay/internal/protocol"
)

func TestValidateUpgradeCompatibilityTreatsLegacyReleaseAsE2EEV1(t *testing.T) {
	legacy := Manifest{}
	target := Manifest{
		AppTransports:   []string{relayprotocol.EncryptedWebSocketSubprotocol},
		RelayTransports: []string{relayprotocol.EncryptedWebSocketSubprotocol},
	}
	if err := ValidateUpgradeCompatibility(legacy, target); err == nil || !strings.Contains(err.Error(), "bridge release") {
		t.Fatalf("compatibility error = %v", err)
	}
}

func TestValidateUpgradeCompatibilityRequiresBothRolloutDirections(t *testing.T) {
	current := Manifest{
		AppTransports:   []string{"transport-v1"},
		RelayTransports: []string{"transport-v1"},
	}
	for name, target := range map[string]Manifest{
		"app before relay": {
			AppTransports:   []string{"transport-v2"},
			RelayTransports: []string{"transport-v1"},
		},
		"relay before app reload": {
			AppTransports:   []string{"transport-v1"},
			RelayTransports: []string{"transport-v2"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := ValidateUpgradeCompatibility(current, target)
			if err == nil || !strings.Contains(err.Error(), "bridge release") {
				t.Fatalf("compatibility error = %v", err)
			}
		})
	}
}

func TestValidateUpgradeCompatibilityAllowsBridgeCutover(t *testing.T) {
	current := Manifest{
		AppTransports:   []string{"transport-v1", "transport-v2"},
		RelayTransports: []string{"transport-v1", "transport-v2"},
	}
	target := Manifest{
		AppTransports:   []string{"transport-v2"},
		RelayTransports: []string{"transport-v2"},
	}
	if err := ValidateUpgradeCompatibility(current, target); err != nil {
		t.Fatal(err)
	}
}

// The hybrid rollout: an installed e2ee-only release must accept the dual
// transport bridge, and must refuse a release that speaks hybrid only.
func TestValidateUpgradeCompatibilityHybridRollout(t *testing.T) {
	current := Manifest{
		AppTransports:   []string{relayprotocol.EncryptedWebSocketSubprotocol},
		RelayTransports: []string{relayprotocol.EncryptedWebSocketSubprotocol},
	}
	bridge := Manifest{
		AppTransports: []string{
			relayprotocol.EncryptedWebSocketSubprotocol,
			relayprotocol.HybridTransportCapability,
		},
		RelayTransports: []string{
			relayprotocol.EncryptedWebSocketSubprotocol,
			relayprotocol.HybridTransportCapability,
		},
	}
	if err := ValidateUpgradeCompatibility(current, bridge); err != nil {
		t.Fatalf("e2ee-only release cannot reach the bridge release: %v", err)
	}

	hybridOnly := Manifest{
		AppTransports:   []string{relayprotocol.HybridTransportCapability},
		RelayTransports: []string{relayprotocol.HybridTransportCapability},
	}
	err := ValidateUpgradeCompatibility(current, hybridOnly)
	if err == nil || !strings.Contains(err.Error(), "bridge release") {
		t.Fatalf("hybrid-only upgrade from an e2ee-only release: error = %v", err)
	}
	if err := ValidateUpgradeCompatibility(bridge, hybridOnly); err != nil {
		t.Fatalf("bridge release cannot reach the hybrid-only release: %v", err)
	}
}
