//go:build herdr_installed_inert_test

package app

import (
	"context"
	"github.com/0cv/herdr-mobile-relay/internal/deviceauth"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
	"testing"
)

// Test build only; never dispatch Run or an external resource by default.
var installedInertRunner func(*Server, context.Context) error

// SetInstalledInertRunner is test-compiled only; it cannot replace orchestration.
func SetInstalledInertRunner(t *testing.T, runner func(*Server, context.Context) error) {
	t.Helper()
	previous := installedInertRunner
	installedInertRunner = runner
	t.Cleanup(func() { installedInertRunner = previous })
}

// InstalledInertState exposes only the constructed state to inert assertions.
// Only external backend readiness is substituted; owned publication and
// acknowledgement still run through the real session and core.
func SetInstalledCommandReadiness(t *testing.T, s *Server) {
	t.Helper()
	if s.installedSetup == nil {
		t.Fatal("setup session missing")
	}
	tailscalecli.SetInstalledSetupReadinessFixture(s.installedSetup.session, func(ctx context.Context) error { return ctx.Err() })
}

func InstalledInertState(t *testing.T, s *Server) (*deviceauth.Store, *tailscalecli.InstalledSession) {
	t.Helper()
	if s.installedCLI == nil || s.deviceStore() == nil || s.bootstrapGate.OpenStatus() || s.pairingControl != nil {
		t.Fatal("installed ownership/store/non-arming policy missing")
	}
	return s.deviceStore(), s.installedCLI
}

func installedCLIRunner() func(*Server, context.Context) error {
	return func(s *Server, ctx context.Context) error {
		if installedInertRunner == nil {
			panic("installed inert runner not configured")
		}
		return installedInertRunner(s, ctx)
	}
}
