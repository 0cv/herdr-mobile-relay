package main

import (
	"context"
	"errors"
	"os"

	"github.com/0cv/herdr-mobile-relay/internal/app"
	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/installedpromotion"
	"github.com/0cv/herdr-mobile-relay/internal/localcontrol"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

// Match every leaf; errors.Is would hide independent joined failures.
func installedCancellationOnly(err error) bool {
	switch e := err.(type) {
	case interface{ Unwrap() []error }:
		children := e.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !installedCancellationOnly(child) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		return installedCancellationOnly(e.Unwrap())
	default:
		return err == context.Canceled
	}
}

var errInstalledCLIUnavailable = errors.New("installed Tailscale CLI is unavailable pending separate qualification and enablement")

func productionInstalledManager(selected config.InstalledConfig) (*tailscalecli.Manager, error) {
	if !config.TailscaleCLIProfilesEnabled() {
		return nil, errInstalledCLIUnavailable
	}
	return tailscalecli.NewInstalledProductionManager(selected.Binding())
}

func runInstalledCLICommand(cfg *config.Config) (int, error) {
	return runInstalledOperatorCommand(cfg, false)
}

func runInstalledSetupCommand() (int, error) {
	switch os.Getenv("HERDR_RELAY_REARM_BOOTSTRAP") {
	case "", "0", "false", "False", "FALSE", "f", "F":
	default:
		return 1, errors.New("installed service refuses bootstrap re-enrollment policy")
	}
	cfg, err := config.LoadInstalled()
	if err != nil || cfg.Transport != config.TransportTailscaleCLI {
		return 1, errors.New("invalid installed service configuration")
	}
	return runInstalledOperatorCommand(cfg, true)
}

func runInstalledOperatorCommand(cfg *config.Config, setup bool) (int, error) {
	selected, err := config.BindInstalledCLI(cfg)
	if err != nil {
		return 1, errors.New("invalid installed service configuration")
	}
	return runInstalledOperatorSelected(selected, setup)
}

func runInstalledPinnedService(args []string) (int, error) {
	if len(args) != 4 || !promotionHex(args[0], 32) || !promotionHex(args[1], 64) || !controlIdentity(args[3]) {
		return 2, errors.New("invalid installed selection reference")
	}
	if !installedPinnedServiceAllowed() {
		return 1, errInstalledCLIUnavailable
	}
	ctx, stop := installedCommandContext()
	selected, err := installedpromotion.Read(ctx, args[2], args[3], installedpromotion.Reference{Generation: args[0], Digest: args[1]})
	stop()
	if err != nil || promotionResumeAdmitted(selected, installedpromotion.Reference{Generation: args[0], Digest: args[1]}) != nil {
		return 1, errors.New("installed selection unavailable")
	}
	return runInstalledOperatorSelected(selected, false)
}

func runInstalledOperatorSelected(selected config.InstalledConfig, setup bool) (int, error) {
	manager, err := installedCommandManager(selected)
	if err != nil {
		if errors.Is(err, errInstalledCLIUnavailable) {
			return 1, errInstalledCLIUnavailable
		}
		return 1, errors.New("installed Tailscale CLI startup refused")
	}
	ctx, stop := installedCommandContext()
	defer stop()
	snapshot := selected.Snapshot()
	logger := newRelayLogger(os.Stderr, snapshot.LogFormat, snapshot.LogLevel, stderrIsJournal(os.Stderr))
	operator := func(ctx context.Context, core localcontrol.InstalledControl, credentials localcontrol.InstalledCredentials) error {
		return installedCommandEndpoint(ctx, selected, core, credentials)
	}
	if setup {
		err = app.RunInstalledSetupOperatorCLI(ctx, selected, version, revision, logger, manager, operator)
	} else {
		err = app.RunInstalledResumeOperatorCLI(ctx, selected, version, revision, logger, manager, operator)
	}
	return installedCommandResult(ctx, err)
}

func installedCommandResult(ctx context.Context, err error) (int, error) {
	if ctx.Err() == context.Canceled && (err == nil || installedCancellationOnly(err)) {
		return 0, nil
	}
	if err != nil || ctx.Err() != nil {
		return 1, errors.New("installed Tailscale CLI startup refused")
	}
	return 0, nil
}
