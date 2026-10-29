//go:build herdr_installed_inert_test

package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

func TestInstalledSetupPublicationClose(t *testing.T) {
	IsolateInstalledFixture(t)
	m, state, coordination, binary, mutations := tailscalecli.NewInstalledSetupInertFixture(t)
	contended := make(chan struct{}, 1)
	tailscalecli.SetInstalledSetupContentionFixture(m, func(name string) {
		if !strings.HasPrefix(name, "instance-") {
			return
		}
		select {
		case contended <- struct{}{}:
		default:
		}
	})
	base := filepath.Dir(state)
	cfg := &config.Config{
		Transport: config.TransportTailscaleCLI, Host: "127.0.0.1", Port: 18377, PluginPort: 18378,
		TailscaleCLIScope: "production", TailscaleCLIOrigin: "https://herdr.tailnet.ts.net:8443",
		InstanceID: "install-fixture", TailscaleCLIStateRoot: state, TailscaleCLICoordinationRoot: coordination, TailscaleCLIBin: binary,
		RuntimeDir: filepath.Join(base, "config", "relay"), CacheDir: filepath.Join(base, "cache"), ConfigHome: filepath.Join(base, "config"), ReleaseRoot: filepath.Join(base, "release"), WebRoot: filepath.Join(base, "web"), PollInterval: 1,
		HerdrBin: filepath.Join(base, "absent-herdr"), SocketPath: filepath.Join(base, "absent-socket"),
		Token: "01234567890123456789012345678901", PhoneAppOrigin: "https://herdr.tailnet.ts.net:8443",
	}
	selected, err := config.BindInstalledCLI(cfg)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	resource := &installedLogicalListener{}
	callbackDone := make(chan struct{})
	SetInstalledInertRunner(t, func(s *Server, ctx context.Context) error {
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		s.installedBackend = newInstalledBackend(ctx, resource)
		defer s.installedBackend.Close()
		readinessCalls := 0
		tailscalecli.SetInstalledSetupReadinessFixture(s.installedSetup.session, func(call context.Context) error {
			readinessCalls++
			if readinessCalls == 1 {
				return nil
			} // pre-consumption preflight; block only dispatched transaction
			close(entered)
			<-call.Done()
			<-release
			return call.Err()
		})
		close(s.listenerBound)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-entered:
		case <-callbackDone:
			return errors.New("callback failed before readiness")
		}
		closed := make(chan struct{})
		go func() { defer close(closed); _ = s.installedBackend.Close() }()
		defer func() { unblock(); <-closed }()
		<-s.installedBackend.lifetime.Done()
		if resource.closed.Load() {
			return errors.New("close escaped publication lease")
		}
		competing, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		callbackEntered := false
		refusal := m.WithInstalledSetupSession(competing, selected.Binding(), func(context.Context, *tailscalecli.InstalledSetupSession) error {
			callbackEntered = true
			return errors.New("duplicate owner entered")
		})
		if callbackEntered || refusal != tailscalecli.ErrUncertain || competing.Err() != context.DeadlineExceeded {
			return errors.New("blocked owner did not return expected expired-context refusal")
		}
		select {
		case <-contended:
		default:
			return errors.New("competing owner did not encounter lock contention")
		}
		if resource.closed.Load() {
			return errors.New("physical close escaped during contention")
		}
		unblock()
		<-closed
		<-callbackDone
		return nil
	})
	err = RunInstalledSetupCLI(context.Background(), selected, "fixture", "fixture", logger, m, func(ctx context.Context, h InstalledSetupControl) error {
		defer close(callbackDone)
		p, err := h.Proposal(ctx)
		if err != nil {
			return err
		}
		err = h.Publish(ctx, tailscalecli.Consent{Accepted: true, Scope: "production", NodeID: p.NodeID, Origin: cfg.TailscaleCLIOrigin, HTTPSPort: 8443, BackendPort: 18377, ReservationID: p.ReservationID, RouteConfirmation: p.Confirmation, PersistentRouteAccepted: true, RouteRemovalAccepted: true, CheckToWriteRaceAccepted: true, PortReuseRiskAccepted: true, NoRollbackAccepted: true, NoRemoteDrainAccepted: true})
		if !errors.Is(err, context.Canceled) {
			return errors.New("close did not cancel publication")
		}
		phase, statusErr := h.Status(context.Background())
		if statusErr != nil || phase != "publication-failed-separate-lifecycle-required" {
			return errors.New("false recovery state")
		}
		if h.Publish(context.Background(), tailscalecli.Consent{}) == nil {
			return errors.New("failed attempt replayed")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resource.closed.Load() || mutations() != 0 {
		t.Fatal("close failed or cancelled readiness dispatched")
	}
}
