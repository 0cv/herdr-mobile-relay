//go:build herdr_installed_inert_test

package app

import (
	"context"
	"errors"
	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/deviceauth"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// Temp filesystem construction only. The private runner never calls Run,
// opens listeners, invokes Herdr, or reads a host credential store.
func TestInstalledCLIConstructionPreservesStore(t *testing.T) {
	root := IsolateInstalledFixture(t)
	cfg := &config.Config{Transport: config.TransportTailscaleCLI, RuntimeDir: filepath.Join(root, "runtime"), CacheDir: filepath.Join(root, "cache"), ConfigHome: filepath.Join(root, "config"), ReleaseRoot: filepath.Join(root, "release"), WebRoot: filepath.Join(root, "web"), PollInterval: 1, HerdrBin: filepath.Join(root, "absent-herdr"), SocketPath: filepath.Join(root, "absent-socket")}
	store, err := deviceauth.Open(filepath.Join(cfg.RuntimeDir, "device-auth"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureBootstrapInvitation([]byte("01234567890123456789012345678901"), "fixture", "en"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.RuntimeDir, "device-auth", "devices.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	stopped := errors.New("inert run failed")
	for i := 0; i < 2; i++ {
		err := runInstalledCLIApp(context.Background(), cfg, "fixture", "fixture", slog.New(slog.NewTextHandler(io.Discard, nil)), nil, func(s *Server, ctx context.Context) error {
			if s.deviceStore() == nil || s.bootstrapGate.OpenStatus() {
				t.Fatal("store missing or admission open")
			}
			if err := s.Run(ctx); !errors.Is(err, tailscalecli.ErrWorkflowRequired) {
				t.Fatalf("nil authority admitted Run: %v", err)
			}
			return stopped
		})
		if !errors.Is(err, stopped) {
			t.Fatal(err)
		}
		after, err := os.ReadFile(path)
		if err != nil || string(before) != string(after) {
			t.Fatalf("store changed: %v", err)
		}
	}
}
