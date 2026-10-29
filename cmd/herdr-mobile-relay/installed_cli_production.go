//go:build !herdr_installed_inert_test

package main

import (
	"context"
	"github.com/0cv/herdr-mobile-relay/internal/localcontrol"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

func installedCommandEndpoint(ctx context.Context, selected config.InstalledConfig, core localcontrol.InstalledControl, credentials localcontrol.InstalledCredentials) error {
	return localcontrol.ServeInstalledEndpoint(ctx, selected, core, credentials)
}

func installedCommandManager(selected config.InstalledConfig) (*tailscalecli.Manager, error) {
	return productionInstalledManager(selected)
}

func installedControlEnabled() bool         { return config.TailscaleCLIProfilesEnabled() }
func installedPinnedServiceAllowed() bool   { return config.TailscaleCLIProfilesEnabled() }
func installedUnpinnedServiceAllowed() bool { return false }
func installedControlDiscover(ctx context.Context, selected config.InstalledConfig) (string, error) {
	s, e := localcontrol.ReadInstalledHandoff(ctx, selected)
	return s.Credentials().Run, e
}
func installedControlNew(ctx context.Context, selected config.InstalledConfig, run string) (installedControlClient, error) {
	return localcontrol.NewInstalledClient(ctx, selected, run)
}

func installedPinnedExecutable() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(path)
}

func installedCommandContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
}
