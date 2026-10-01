//go:build herdr_tailscale_test && herdr_tailscale_cli_fixture_binary

package app

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

// DevelopmentCLIFixtureHooks are available only in the explicitly tagged
// native command-fixture test binary. They keep trust, bundle, and control
// callback observation local to a single synthetic foreground server.
type DevelopmentCLIFixtureHooks struct {
	HealthClient            func(time.Duration) *http.Client
	VerifyPublicBundle      func(context.Context, string, string, string, string) error
	ControlCallbackObserver func(string)
	MarkInventoryReady      bool
}

// NewDevelopmentCLIWithFixtureHooks constructs the real CLI development app
// and attaches test-local HTTPS, bundle-verification, and callback hooks.
func NewDevelopmentCLIWithFixtureHooks(cfg *config.Config, version, revision string, logger *slog.Logger,
	workflow *tailscalecli.DevelopmentWorkflow, hooks DevelopmentCLIFixtureHooks) (*Server, error) {
	server, err := NewDevelopmentCLI(cfg, version, revision, logger, workflow)
	if err != nil {
		return nil, err
	}
	server.developmentCLIHealthClient = hooks.HealthClient
	server.verifyPublicBundle = hooks.VerifyPublicBundle
	server.developmentCLIControlObserver = hooks.ControlCallbackObserver
	if hooks.MarkInventoryReady {
		server.state.CommitInventory(nil, 0)
	}
	return server, nil
}
