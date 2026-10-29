//go:build !linux && !darwin

package localcontrol

import (
	"context"
	"github.com/0cv/herdr-mobile-relay/internal/config"
	"testing"
)

func TestInstalledHandoffPreflightUnsupported(t *testing.T) {
	got, e := InspectInstalledHandoff(context.Background(), config.InstalledConfig{})
	if e == nil || got != (InstalledHandoffObservation{}) {
		t.Fatal("unsupported fallback")
	}
}
