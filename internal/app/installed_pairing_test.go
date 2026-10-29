//go:build herdr_installed_inert_test

package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/deviceauth"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

func TestInstalledPairingConcreteRefusals(t *testing.T) {
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

	// Seed synthetic durable bytes before the concrete app owns its sole store.
	seed, err := deviceauth.OpenDeferred(filepath.Join(cfg.RuntimeDir, "device-auth"))
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.ArmBootstrapInvitationTransactional([]byte("synthetic-bootstrap-secret-00001"), "fixture", "en"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.RuntimeDir, "device-auth", "devices.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wrongConfig := selected.Snapshot()
	wrongConfig.Token = "synthetic-bootstrap-secret-00002"
	wrongSelection, err := config.BindInstalledCLI(&wrongConfig)
	if err != nil {
		t.Fatal(err)
	}
	var retained *Server
	// Each real operation must reach admission on a fresh owned generation:
	// absent UDP/local readiness refuses transiently before profile/bundle checks.
	var admissionArm bool
	SetInstalledInertRunner(t, func(s *Server, ctx context.Context) error {
		retained = s
		s.developmentCLIHealthClient = func(time.Duration) *http.Client { return &http.Client{Transport: installedTerminalDenyHTTP{}} }
		store := s.deviceStore()
		if store == nil {
			return errors.New("store absent")
		}
		unchanged := func() bool {
			after, err := os.ReadFile(path)
			return err == nil && bytes.Equal(before, after) && s.deviceStore() == store
		}
		for _, arm := range []bool{false, true} {
			if p, _, err := s.installedPairing(ctx, selected, arm); err == nil || p.URL != "" || !unchanged() {
				return errors.New("missing lease accepted")
			}
		}
		s.installedBackend = newInstalledBackend(ctx, &installedLogicalListener{})
		defer s.installedBackend.Close()
		if err := s.installedPairingResultBoundary(ctx, selected); err != nil {
			return errors.New("live concrete owner refused at boundary")
		}
		for _, arm := range []bool{false, true} {
			if p, _, err := s.installedPairing(ctx, wrongSelection, arm); err == nil || p.URL != "" || !unchanged() {
				return errors.New("wrong selection accepted")
			}
		}
		if s.installedAdmissionRevoked() {
			return errors.New("fresh generation already terminal")
		}
		if p, status, err := s.installedPairing(ctx, selected, admissionArm); !errors.Is(err, tailscalecli.ErrTransientUnavailable) || status.LocalReady || status.Ready || p != (installedPairingPayload{}) || !unchanged() {
			return errors.New("unqualified admission did not preserve transient refusal")
		}
		if s.installedAdmissionRevoked() {
			return errors.New("transient unqualified observation latched")
		}
		// Queue cancellation is independently exercised with confirmed entry by
		// TestInstalledPairingScopeQueuedCancellation.
		s.recordInstalledRecoveryFailure(ctx, tailscalecli.ErrConflict)
		for _, arm := range []bool{false, true} {
			if p, _, err := s.installedPairing(ctx, selected, arm); err == nil || p.URL != "" || !unchanged() {
				return errors.New("terminal admission accepted")
			}
		}
		return nil
	})
	for _, arm := range []bool{false, true} {
		admissionArm = arm
		previousGeneration := retained
		if err := RunInstalledCLI(context.Background(), selected, "fixture", "fixture", slog.New(slog.NewTextHandler(io.Discard, nil)), m); err != nil {
			t.Fatal(err)
		}
		if retained == previousGeneration {
			t.Fatal("server generation reused")
		}
	}
	if retained == nil {
		t.Fatal("runner absent")
	}
	if p, _, err := retained.reprintInstalledPairing(context.Background(), selected); err == nil || p.URL != "" {
		t.Fatal("retired owner accepted")
	}
	// This boundary helper does not consult the terminal latch. Give the
	// retained concrete owner an otherwise active logical backend: refusal
	// must therefore come from the retired original session, not old close.
	retained.installedBackend = newInstalledBackend(context.Background(), &installedLogicalListener{})
	defer retained.installedBackend.Close()
	if err := retained.installedPairingResultBoundary(context.Background(), selected); err == nil {
		t.Fatal("retired concrete owner with active backend accepted")
	}
}

func TestInstalledPairingURLActualSecretContract(t *testing.T) {
	raw := "synthetic-bootstrap-secret-00001"
	inv := deviceauth.BootstrapInvitationSnapshot{InvitationID: "bootstrap", Version: 1, Secret: base64.RawURLEncoding.EncodeToString([]byte(raw))}
	got, err := installedInvitationPayload(inv, "https://relay.example", "https://phone.example", "Synthetic")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(got.URL)
	if err != nil {
		t.Fatal("URL invalid")
	}
	fragment, err := url.ParseQuery(u.Fragment)
	if err != nil {
		t.Fatal("fragment invalid")
	}
	if fragment.Get("setup") != raw || fragment.Get("relay") != "wss://relay.example" {
		t.Fatal("raw token contract mismatch")
	}
	for _, value := range []string{fmt.Sprint(got), fmt.Sprintf("%#v", got)} {
		if strings.Contains(value, raw) || strings.Contains(value, inv.Secret) {
			t.Fatal("format leaked")
		}
	}
	for _, secret := range []string{"bad", base64.RawURLEncoding.EncodeToString(make([]byte, 32))} {
		inv.Secret = secret
		if got, err := installedInvitationPayload(inv, "https://relay.example", "https://phone.example", "Synthetic"); err == nil || got.URL != "" {
			t.Fatal("unrepresentable secret accepted")
		}
	}
}

// Logical close only: no socket, listener syscall or detached timeout worker.
func TestInstalledPairingBackendLeaseDrain(t *testing.T) {
	IsolateInstalledFixture(t)
	resource := &installedLogicalListener{}
	b := newInstalledBackend(context.Background(), resource)
	entered, release, done, closed := make(chan struct{}), make(chan struct{}), make(chan error, 1), make(chan error, 1)
	go func() {
		done <- (&Server{installedBackend: b}).withInstalledPairingScope(context.Background(), func(ctx context.Context) error { close(entered); <-ctx.Done(); <-release; return nil })
	}()
	<-entered
	go func() { closed <- b.Close() }()
	<-b.lifetime.Done()
	prematureClose := resource.closed.Load()
	premature := false
	select {
	case <-closed:
		premature = true
	default:
	}
	close(release)
	leaseErr := <-done
	if !premature {
		if err := <-closed; err != nil {
			t.Error("close failed")
		}
	}
	if prematureClose || premature || leaseErr != nil {
		t.Fatal("lease did not drain before close")
	}
	if err := b.WithLease(context.Background(), func() error { return nil }); err == nil {
		t.Fatal("closed lease accepted")
	}
}
