//go:build !linux && !darwin

package localcontrol

import (
	"context"
	"github.com/0cv/herdr-mobile-relay/internal/config"
)

func InspectInstalledHandoff(context.Context, config.InstalledConfig) (InstalledHandoffObservation, error) {
	return InstalledHandoffObservation{}, errUnsupportedHandoff
}
