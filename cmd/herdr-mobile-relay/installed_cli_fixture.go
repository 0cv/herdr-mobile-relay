//go:build herdr_installed_inert_test

package main

import (
	"context"
	"errors"
	"github.com/0cv/herdr-mobile-relay/internal/localcontrol"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

// Synthetic manager/context injection exists only in this inert build.
// Without an explicitly installed fixture the ordinary refusal still applies.
var installedFixtureManager func(config.InstalledConfig) (*tailscalecli.Manager, error)
var installedFixtureContext context.Context
var installedFixturePinnedExecutable string

func installedPinnedExecutable() (string, error) {
	if installedFixturePinnedExecutable == "" {
		return "", errPromotion
	}
	return installedFixturePinnedExecutable, nil
}

var installedFixtureEndpoint func(context.Context, config.InstalledConfig, localcontrol.InstalledControl, localcontrol.InstalledCredentials) error

func installedCommandEndpoint(ctx context.Context, selected config.InstalledConfig, core localcontrol.InstalledControl, credentials localcontrol.InstalledCredentials) error {
	if installedFixtureEndpoint == nil {
		return errors.New("installed endpoint fixture unavailable")
	}
	return installedFixtureEndpoint(ctx, selected, core, credentials)
}

func installedCommandManager(selected config.InstalledConfig) (*tailscalecli.Manager, error) {
	if installedFixtureManager == nil {
		return productionInstalledManager(selected)
	}
	return installedFixtureManager(selected)
}

// Scripted substitution never falls through to native IO.
var installedControlFixtureDiscover func(context.Context, config.InstalledConfig) (string, error)
var installedControlFixtureNew func(context.Context, config.InstalledConfig, string) (installedControlClient, error)

func installedControlEnabled() bool         { return installedControlFixtureNew != nil }
func installedPinnedServiceAllowed() bool   { return installedFixtureManager != nil }
func installedUnpinnedServiceAllowed() bool { return installedFixtureManager != nil }
func installedControlDiscover(ctx context.Context, s config.InstalledConfig) (string, error) {
	if installedControlFixtureDiscover == nil {
		return "", errControlRefused
	}
	return installedControlFixtureDiscover(ctx, s)
}
func installedControlNew(ctx context.Context, s config.InstalledConfig, run string) (installedControlClient, error) {
	if installedControlFixtureNew == nil {
		return nil, errControlRefused
	}
	return installedControlFixtureNew(ctx, s, run)
}

func installedCommandContext() (context.Context, context.CancelFunc) {
	if installedFixtureContext == nil {
		panic("installed inert context not configured")
	}
	return context.WithCancel(installedFixtureContext)
}
