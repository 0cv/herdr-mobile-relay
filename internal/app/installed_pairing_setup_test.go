//go:build herdr_installed_inert_test

package app

import (
	"context"
	"errors"
	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestInstalledPairingConcreteUnregisteredRetiredSetup(t *testing.T) {
	IsolateInstalledFixture(t)
	previous := http.DefaultTransport
	http.DefaultTransport = installedTerminalDenyHTTP{}
	t.Cleanup(func() { http.DefaultTransport = previous })
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
	selected, err := config.BindInstalledCLI(cfg)
	if err != nil {
		t.Fatal(err)
	}

	server := make(chan *Server, 1)
	SetInstalledInertRunner(t, func(s *Server, ctx context.Context) error {
		s.developmentCLIHealthClient = func(time.Duration) *http.Client { return &http.Client{Transport: installedTerminalDenyHTTP{}} }
		s.installedBackend = newInstalledBackend(ctx, &installedLogicalListener{})
		defer s.installedBackend.Close()
		server <- s
		close(s.listenerBound)
		<-s.installedSetup.lifetime.Done() // callback retired; physical backend remains active
		for _, arm := range []bool{false, true} {
			if p, _, err := s.installedPairing(ctx, selected, arm); err == nil || p.URL != "" {
				return errors.New("retired setup accepted")
			}
		}
		return nil
	})
	control := func(ctx context.Context, _ InstalledSetupControl) error {
		s := <-server
		store := s.deviceStore()
		for _, arm := range []bool{false, true} {
			if p, _, err := s.installedPairing(ctx, selected, arm); err == nil || p.URL != "" {
				return errors.New("unregistered setup accepted")
			}
			if p, _, err := s.installedPairing(ctx, config.InstalledConfig{}, arm); err == nil || p.URL != "" {
				return errors.New("wrong setup selection accepted")
			}
		}
		if s.deviceStore() != store || store.BootstrapStatus().Armed {
			return errors.New("setup refusal changed store")
		}
		return nil
	}
	if err := RunInstalledSetupCLI(context.Background(), selected, "fixture", "fixture", slog.New(slog.NewTextHandler(io.Discard, nil)), m, control); err != nil {
		t.Fatal(err)
	}
	if mutations() != 0 {
		t.Fatal("refusal published")
	}
	if _, err := os.Lstat(filepath.Join(cfg.RuntimeDir, "device-auth", "devices.json")); !os.IsNotExist(err) {
		t.Fatal("refusal wrote store")
	}
}
