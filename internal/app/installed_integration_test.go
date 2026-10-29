//go:build herdr_installed_inert_test

package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/deviceauth"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

// Actual public entrypoint, concrete manager/session and app/store construction;
// the test-only runner prevents listeners, Herdr and all other Run effects.
func TestInstalledConcreteAppLifecycle(t *testing.T) {
	IsolateInstalledFixture(t)
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
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runWithManager := func(ctx context.Context, manager *tailscalecli.Manager) error {
		selected, err := config.BindInstalledCLI(cfg)
		if err != nil {
			return err
		}
		return RunInstalledCLI(ctx, selected, "fixture", "fixture", logger, manager)
	}
	run := func(ctx context.Context) error { return runWithManager(ctx, m) }
	defer func() { installedInertRunner = nil }()
	installedInertRunner = func(*Server, context.Context) error { t.Fatal("binding mismatch constructed app"); return nil }
	original := cfg.RuntimeDir
	cfg.RuntimeDir = filepath.Join(state, "wrong")
	if err := run(context.Background()); err == nil {
		t.Fatal("binding mismatch accepted")
	}
	if _, err := os.Stat(cfg.RuntimeDir); !os.IsNotExist(err) {
		t.Fatalf("mismatch resource: %v", err)
	}
	cfg.RuntimeDir = original
	for _, mutate := range []func(*config.Config){
		func(c *config.Config) { c.TailscaleCLIScope = "development" },
		func(c *config.Config) { c.Token = "short" },
		func(c *config.Config) { c.PortMappingEnabled = true },
		func(c *config.Config) { c.RearmBootstrap = true },
		func(c *config.Config) { c.ReleaseRoot = c.RuntimeDir },
		func(c *config.Config) { c.Port += 10 },
		func(c *config.Config) { c.TailscaleCLIOrigin = "https://herdr.tailnet.ts.net:9443" },
		func(c *config.Config) { c.InstanceID = "other-instance" },
		func(c *config.Config) { c.TailscaleCLIStateRoot += "-other" },
		func(c *config.Config) { c.TailscaleCLICoordinationRoot += "-other" },
		func(c *config.Config) { c.TailscaleCLIBin += "-other" },
		func(c *config.Config) { c.CacheDir = c.RuntimeDir },
	} {
		snapshot := *cfg
		mutate(cfg)
		if err := run(context.Background()); err == nil {
			t.Fatal("binding mismatch accepted")
		}
		*cfg = snapshot
	}
	otherManager, _, _, _ := tailscalecli.NewInstalledInertFixture(t)
	if err := runWithManager(context.Background(), otherManager); err == nil {
		t.Fatal("wrong manager constructed app")
	}
	if _, err := os.Stat(cfg.RuntimeDir); !os.IsNotExist(err) {
		t.Fatalf("binding mismatch created runtime: %v", err)
	}

	for _, root := range []string{cfg.ConfigHome, cfg.RuntimeDir, cfg.CacheDir, cfg.ReleaseRoot} {
		if err := os.MkdirAll(root, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(cfg.RuntimeDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background()); err == nil {
		t.Fatal("unsafe runtime constructed app")
	}
	// Fixture metadata reset only; the binding never repairs permissions.
	if err := os.Chmod(cfg.RuntimeDir, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := deviceauth.Open(filepath.Join(cfg.RuntimeDir, "device-auth"))
	if err != nil {
		t.Fatal(err)
	}
	selectors := []transport.E2EEAuthSelector{}
	secrets := [][]byte{}
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
		if _, err := store.CompleteE2EEAuth(context.Background(), selector, true); err != nil {
			t.Fatal(err)
		}
		secret, err := store.ResolveE2EESecret(context.Background(), selector)
		if err != nil {
			t.Fatal(err)
		}
		selectors = append(selectors, selector)
		secrets = append(secrets, secret)
	}
	credentials := store.ListCredentials("")
	path := filepath.Join(cfg.RuntimeDir, "device-auth", "devices.json")
	read := func(p string) []byte {
		t.Helper()
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	before := read(path)
	journal := read(filepath.Join(state, "registration.json"))
	stopped := errors.New("inert runner failed")
	for _, outcome := range []error{nil, stopped, nil} {
		installedInertRunner = func(s *Server, ctx context.Context) error {
			if s.installedCLI == nil || s.deviceStore() == nil || s.bootstrapGate.OpenStatus() || s.pairingControl != nil {
				t.Fatal("ownership/store/admission")
			}
			if !reflect.DeepEqual(credentials, s.deviceStore().ListCredentials("")) {
				t.Fatal("credential or role changed")
			}
			for i, selector := range selectors {
				secret, err := s.deviceStore().ResolveE2EESecret(ctx, selector)
				if err != nil || !bytes.Equal(secret, secrets[i]) {
					t.Fatalf("credential secret changed: %v", err)
				}
			}
			competing, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
			defer cancel()
			if err := run(competing); err == nil {
				t.Fatal("competing owner entered while store open")
			}
			return outcome
		}
		if err := run(context.Background()); !errors.Is(err, outcome) {
			t.Fatalf("run: %v", err)
		}
		if !bytes.Equal(before, read(path)) || !bytes.Equal(journal, read(filepath.Join(state, "registration.json"))) {
			t.Fatal("persistent state changed")
		}
	}
	// A corrupt device file fails construction, never dispatches the runner, and
	// releases the concrete ownership so a restored file can be reopened.
	if err := os.WriteFile(path, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	installedInertRunner = func(*Server, context.Context) error { t.Fatal("constructed corrupt store"); return nil }
	if err := run(context.Background()); err == nil {
		t.Fatal("corrupt store accepted")
	}
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	installedInertRunner = func(*Server, context.Context) error { return nil }
	if err := run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, read(path)) {
		t.Fatal("bootstrap rearmed")
	}
	// Missing leaves are prepared privately only inside the acquired session.
	cfg.RuntimeDir = filepath.Join(base, "config", "new-relay")
	cfg.CacheDir = filepath.Join(base, "new-cache")
	if err := run(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{cfg.RuntimeDir, cfg.CacheDir} {
		info, err := os.Lstat(root)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0700 {
			t.Fatal("new leaf is not private")
		}
	}
	if !bytes.Equal(before, read(path)) {
		t.Fatal("original credentials changed")
	}
	for _, leaf := range []string{"runtime", "cache", "device-auth"} {
		if _, err := os.Lstat(filepath.Join(state, leaf)); !os.IsNotExist(err) {
			t.Fatalf("relocated state: %v", err)
		}
	}
}
