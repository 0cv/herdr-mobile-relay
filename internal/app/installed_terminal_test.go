//go:build herdr_installed_inert_test

package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

type installedTerminalDenyHTTP struct{}

func (installedTerminalDenyHTTP) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("inert egress denied")
}

// Concrete registered owner and constructor; never Run, sockets or qualification.
func TestInstalledConcreteTerminalAdmissionLatch(t *testing.T) {
	IsolateInstalledFixture(t)
	previous := http.DefaultTransport
	http.DefaultTransport = installedTerminalDenyHTTP{}
	t.Cleanup(func() { http.DefaultTransport = previous })
	m, state, coordination, binary := tailscalecli.NewInstalledInertFixture(t)
	base := filepath.Dir(state)
	cfg := &config.Config{
		Transport: config.TransportTailscaleCLI, Host: "127.0.0.1", Port: 18377, PluginPort: 18378,
		TailscaleCLIScope: "production", TailscaleCLIOrigin: "https://herdr.tailnet.ts.net:8443",
		InstanceID: "install-fixture", TailscaleCLIStateRoot: state, TailscaleCLICoordinationRoot: coordination, TailscaleCLIBin: binary,
		RuntimeDir: filepath.Join(base, "config", "relay"), CacheDir: filepath.Join(base, "cache"), ConfigHome: filepath.Join(base, "config"), ReleaseRoot: filepath.Join(base, "release"), WebRoot: filepath.Join(base, "web"), PollInterval: 1,
		HerdrBin: filepath.Join(base, "absent-herdr"), SocketPath: filepath.Join(base, "absent-socket"),
		Token: "01234567890123456789012345678901", PhoneAppOrigin: "https://herdr.tailnet.ts.net:8443",
	}
	for _, p := range []string{cfg.ConfigHome, cfg.RuntimeDir, cfg.CacheDir, cfg.ReleaseRoot} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	selected, err := config.BindInstalledCLI(cfg)
	if err != nil {
		t.Fatal(err)
	}
	SetInstalledInertRunner(t, func(s *Server, ctx context.Context) error {
		s.developmentCLIHealthClient = func(time.Duration) *http.Client { return &http.Client{Transport: installedTerminalDenyHTTP{}} }
		resource := &installedLogicalListener{}
		s.installedBackend = newInstalledBackend(ctx, resource)
		defer s.installedBackend.Close()
		store := s.deviceStore()
		if store == nil || s.installedCLI == nil {
			return errors.New("missing concrete owner/store")
		}
		// Availability remains reversible; cancellation is not terminal evidence.
		s.recordInstalledRecoveryFailure(ctx, tailscalecli.ErrTransientUnavailable)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		s.recordInstalledRecoveryFailure(cancelled, tailscalecli.ErrConflict)
		if s.installedAdmissionRevoked() {
			return errors.New("transient or cancellation latched")
		}
		// Joined availability cannot hide a conflict (ISA-001 precedence).
		s.recordInstalledRecoveryFailure(ctx, errors.Join(tailscalecli.ErrTransientUnavailable, tailscalecli.ErrConflict))
		s.recordInstalledRecoveryFailure(ctx, nil)
		if !s.installedAdmissionRevoked() {
			return errors.New("terminal result not latched")
		}
		if s.bootstrapGate.ResumeAdmission() == nil {
			return errors.New("revoked gate reopened")
		}
		for i := 0; i < 2; i++ {
			if !errors.Is(s.checkTailscaleCLIReadiness(ctx), tailscalecli.ErrWorkflowRequired) {
				return errors.New("direct readiness bypassed terminal latch")
			}
			if _, err := s.admitTailscaleCLI(ctx); err == nil {
				return errors.New("direct admission reopened")
			}
			if _, err := s.armTailscaleCLI(ctx); err == nil {
				return errors.New("direct arm reopened")
			}
		}
		if s.deviceStore() != store || s.bootstrapGate.OpenStatus() || store.BootstrapStatus().Armed {
			return errors.New("terminal refusal changed store or armed")
		}
		return nil
	})
	if err := RunInstalledCLI(context.Background(), selected, "fixture", "fixture", slog.New(slog.NewTextHandler(io.Discard, nil)), m); err != nil {
		t.Fatal(err)
	}
}
