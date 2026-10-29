//go:build herdr_installed_inert_test

package main

import (
	"context"
	"os"
	"syscall"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/release"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

func promotionBackendEligible(cfg config.Config, manifest release.Manifest) error {
	if cfg.InstanceID == "" || manifest.Version == "" || manifest.WebHash == "" {
		return errPromotion
	}
	return nil
}

// The logical transport has no HTTP listener. The service fixture separately
// witnesses STRICT backend acquisition and preserved store after the pin changes.
func promotionBackendReady(ctx context.Context, cfg config.Config, manifest release.Manifest) error {
	if ctx.Err() != nil || cfg.InstanceID == "" || manifest.Version == "" || manifest.WebHash == "" {
		return errPromotion
	}
	return nil
}

// The inert manager supplies only logical route/readiness observations; it
// cannot and must not claim physical runtime qualification for this host.
func promotionRouteQualification(m *tailscalecli.Manager, route tailscalecli.RouteStatus) bool {
	return m != nil && route.Readiness == tailscalecli.ReadinessReady
}

func ownedPromotionDir(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

// Only external native effects may be substituted in the inert test build.
var installedPromotionFixtureNative func(config.InstalledConfig) (promotionNative, error)

func installedPromotionNative(selected config.InstalledConfig) (promotionNative, error) {
	if installedPromotionFixtureNative == nil {
		return nil, errPromotion
	}
	return installedPromotionFixtureNative(selected)
}
