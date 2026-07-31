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
	if err := ValidateUpgradeCompatibility(legacy, target); err != nil {
		t.Fatal(err)
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
