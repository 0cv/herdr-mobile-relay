//go:build herdr_installed_inert_test

package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/deviceauth"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

type installedLogicalListener struct{ closed atomic.Bool }

func (*installedLogicalListener) Accept() (net.Conn, error) {
	return nil, errors.New("inert resource cannot accept")
}
func (*installedLogicalListener) Addr() net.Addr { return nil }
func (l *installedLogicalListener) Close() error { l.closed.Store(true); return nil }

func TestInstalledSetupConcreteApp(t *testing.T) {
	IsolateInstalledFixture(t)
	m, state, coordination, binary, mutations := tailscalecli.NewInstalledSetupInertFixture(t)
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
	// Existing credentials are fixture data, not route registration authority.
	store, err := deviceauth.Open(filepath.Join(cfg.RuntimeDir, "device-auth"))
	if err != nil {
		t.Fatal(err)
	}
	var selectors []transport.E2EEAuthSelector
	var secrets [][]byte
	for _, role := range []deviceauth.Role{deviceauth.RoleController, deviceauth.RoleReader} {
		invitation, err := store.CreateInvitation(string(role), role, "en")
		if err != nil {
			t.Fatal(err)
		}
		result, err := store.CompleteE2EEAuth(context.Background(), transport.E2EEAuthSelector{Kind: transport.E2EEAuthInvitation, ID: invitation.InvitationID, Version: invitation.Version}, true)
		if err != nil {
			t.Fatal(err)
		}
		selector := transport.E2EEAuthSelector{Kind: transport.E2EEAuthCredential, ID: result.Identity.CredentialID, Version: result.Identity.CredentialVersion, Locale: result.Identity.Locale}
		secret, err := store.ResolveE2EESecret(context.Background(), selector)
		if err != nil {
			t.Fatal(err)
		}
		selectors = append(selectors, selector)
		secrets = append(secrets, secret)
	}
	if _, err := store.CreateInvitation("pending", deviceauth.RoleReader, "en"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.RuntimeDir, "device-auth", "devices.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := config.BindInstalledCLI(cfg)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	constructed := false
	SetInstalledInertRunner(t, func(*Server, context.Context) error {
		constructed = true
		return errors.New("refused selection constructed app")
	})
	other, _, _, _, _ := tailscalecli.NewInstalledSetupInertFixture(t)
	noop := func(context.Context, InstalledSetupControl) error { return nil }
	if RunInstalledSetupCLI(context.Background(), selected, "fixture", "fixture", logger, other, noop) == nil || constructed {
		t.Fatal("wrong manager reached app construction")
	}
	// Drift the ORIGINAL selected pathname; never rebind it to the replacement.
	func() {
		original := cfg.ReleaseRoot + "-original"
		if err := os.Rename(cfg.ReleaseRoot, original); err != nil {
			t.Fatal(err)
		}
		defer func() {
			_ = os.Remove(cfg.ReleaseRoot)
			if err := os.Rename(original, cfg.ReleaseRoot); err != nil {
				t.Error(err)
			}
		}()
		if err := os.Symlink(original, cfg.ReleaseRoot); err != nil {
			t.Fatal(err)
		}
		if RunInstalledSetupCLI(context.Background(), selected, "fixture", "fixture", logger, m, noop) == nil || constructed {
			t.Fatal("alias drift reached app construction")
		}
	}()
	var retained InstalledSetupControl
	callbackDone := make(chan struct{})
	resource := &installedLogicalListener{}
	SetInstalledInertRunner(t, func(s *Server, ctx context.Context) error {
		if s.installedSetup == nil || s.bootstrapGate.OpenStatus() || s.pairingControl != nil {
			return errors.New("missing closed setup owner")
		}
		if _, err := os.Stat(filepath.Join(state, "registration.json")); !os.IsNotExist(err) {
			return errors.New("preseeded registration")
		}
		for i, selector := range selectors {
			secret, err := s.deviceStore().ResolveE2EESecret(ctx, selector)
			if err != nil || !bytes.Equal(secret, secrets[i]) {
				return errors.New("credential secret changed")
			}
		}
		if s.tailscaleCLIControlStatus(ctx).Ready || mutations() != 0 {
			return errors.New("waiting admitted or mutated")
		}
		s.installedBackend = newInstalledBackend(ctx, resource)
		defer s.installedBackend.Close()
		tailscalecli.SetInstalledSetupReadinessFixture(s.installedSetup.session, func(context.Context) error {
			if resource.closed.Load() {
				return errors.New("readiness after physical close")
			}
			// Exercises the publication-readiness lock cycle without HTTP.
			if s.tailscaleCLIControlStatus(ctx).Ready {
				return errors.New("publication armed admission")
			}
			return nil
		})
		close(s.listenerBound)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.installedSetup.published:
		case <-callbackDone:
			if !s.installedSetup.registered() {
				return errors.New("callback failed before publication")
			}
		}
		<-s.installedSetup.lifetime.Done() // callback retired, service must remain alive
		if ctx.Err() != nil {
			return errors.New("operator disconnect stopped service")
		}
		if s.bootstrapGate.OpenStatus() || s.tailscaleCLIControlStatus(ctx).Ready {
			return errors.New("unqualified publication admitted")
		}
		return nil
	})
	err = RunInstalledSetupCLI(context.Background(), selected, "fixture", "fixture", logger, m, func(ctx context.Context, h InstalledSetupControl) error {
		defer close(callbackDone)
		retained = h
		p, err := h.Proposal(ctx)
		if err != nil {
			return err
		}
		return h.Publish(ctx, tailscalecli.Consent{Accepted: true, Scope: "production", NodeID: p.NodeID, Origin: cfg.TailscaleCLIOrigin, HTTPSPort: 8443, BackendPort: 18377, ReservationID: p.ReservationID, RouteConfirmation: p.Confirmation, PersistentRouteAccepted: true, RouteRemovalAccepted: true, CheckToWriteRaceAccepted: true, PortReuseRiskAccepted: true, NoRollbackAccepted: true, NoRemoteDrainAccepted: true})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resource.closed.Load() || mutations() != 1 {
		t.Fatal("lease retirement/publication count")
	}
	if _, err := retained.Proposal(context.Background()); err == nil {
		t.Fatal("retained control accepted")
	}
	if (InstalledSetupControl{}).Publish(context.Background(), tailscalecli.Consent{}) == nil {
		t.Fatal("zero control accepted")
	}
	SetInstalledInertRunner(t, func(s *Server, ctx context.Context) error {
		if s.installedCLI == nil || s.bootstrapGate.OpenStatus() {
			return errors.New("resume authority/admission")
		}
		return nil
	})
	if err := RunInstalledCLI(context.Background(), selected, "fixture", "fixture", logger, m); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || mutations() != 1 {
		t.Fatal("resume changed credentials/invitations/route")
	}
}

func TestInstalledBackendCloseDrains(t *testing.T) {
	// No app, filesystem, network, speech or process effects in this case.
	resource := &installedLogicalListener{}
	var missing *installedBackend
	if missing.WithLease(context.Background(), func() error { t.Error("missing lease entered"); return nil }) == nil {
		t.Fatal("missing lease accepted")
	}
	var inactive installedBackend
	if inactive.WithLease(context.Background(), func() error { t.Error("inactive lease entered"); return nil }) == nil {
		t.Fatal("inactive lease accepted")
	}
	b := newInstalledBackend(context.Background(), resource)
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		_ = b.WithLease(context.Background(), func() error { close(entered); <-release; return nil })
	}()
	<-entered
	closed := make(chan struct{})
	go func() { defer close(closed); _ = b.Close() }()
	// Retain all isolation until both owned goroutines have joined on failure.
	defer func() { close(release); <-finished; <-closed }()
	<-b.lifetime.Done()
	if resource.closed.Load() {
		t.Fatal("physical close bypassed lease")
	}
	if b.WithLease(context.Background(), func() error { t.Error("retired lease entered"); return nil }) == nil {
		t.Fatal("retired lease accepted")
	}
}
