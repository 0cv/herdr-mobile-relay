package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/tailscale"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

// appAuthorityFixture represents only the prepared, not-dispatched boundary
// needed by negative startup tests. It never reports an active/validated route
// or supplies ready/owned facts. Production SessionAuthority behavior and
// byte-level LocalAPI protocol cases are tested in internal/tailscale.
type appAuthorityFixture struct {
	mu              sync.Mutex
	status          tailscale.AuthorityStatus
	origin          string
	activationCalls int
	validationCalls int
	retirementCalls int
	invalidation    chan struct{}
	activationErr   error
	validationErr   error
	retirementErr   error
}

func newAppAuthorityFixture() *appAuthorityFixture {
	return &appAuthorityFixture{
		status:       tailscale.AuthorityStatus{Prepared: true, RemoteWatchRetirementUnknown: true, RegistrationOutcome: "not-dispatched"},
		invalidation: make(chan struct{}),
	}
}

func (a *appAuthorityFixture) Activate(context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.activationCalls++
	return a.activationErr
}

func (a *appAuthorityFixture) Validate(context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.validationCalls++
	return a.validationErr
}

func (a *appAuthorityFixture) Retire(context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.retirementCalls++
	return a.retirementErr
}

func (a *appAuthorityFixture) Invalidation() <-chan struct{} { return a.invalidation }

func (a *appAuthorityFixture) Status() tailscale.AuthorityStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.status
}

func (a *appAuthorityFixture) Origin() (string, bool) { return a.origin, a.origin != "" }

func (a *appAuthorityFixture) activationCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.activationCalls
}

func managedTailscaleFixtureConfig(root string) *config.Config {
	return &config.Config{
		Host:            "127.0.0.1",
		Port:            18991,
		PluginPort:      0,
		SocketPath:      filepath.Join(root, "herdr.sock"),
		PollInterval:    3600,
		RuntimeDir:      root,
		CacheDir:        filepath.Join(root, "cache"),
		ConfigHome:      filepath.Join(root, "config"),
		WebRoot:         filepath.Join(root, "missing-web-root"),
		HerdrBin:        filepath.Join(root, "missing-herdr"),
		Token:           strings.Repeat("k", 32),
		Transport:       config.TransportTailscale,
		TailscaleOrigin: "https://relay.example.ts.net",
		InstanceID:      "instance-managed",
		ManagedRunID:    "run-managed",
	}
}

func TestManagedTailscaleConstructorLeavesDeviceStoreUntouched(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "absent"
		if existing {
			name = "existing"
		}
		t.Run(name, func(t *testing.T) {
			root := managedTestRoot(t)
			deviceDir := filepath.Join(root, "device-auth")
			deviceFile := filepath.Join(deviceDir, "devices.json")
			var beforeBytes []byte
			var beforeDirMode, beforeFileMode os.FileMode
			if existing {
				if err := os.Mkdir(deviceDir, 0o755); err != nil {
					t.Fatal(err)
				}
				beforeBytes = []byte("preserve this pre-existing device state exactly\n")
				if err := os.WriteFile(deviceFile, beforeBytes, 0o640); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(deviceDir, 0o755); err != nil {
					t.Fatal(err)
				}
				beforeDirInfo, err := os.Stat(deviceDir)
				if err != nil {
					t.Fatal(err)
				}
				beforeFileInfo, err := os.Stat(deviceFile)
				if err != nil {
					t.Fatal(err)
				}
				beforeDirMode, beforeFileMode = beforeDirInfo.Mode().Perm(), beforeFileInfo.Mode().Perm()
			}

			owner, err := AcquireManagedOwner(root)
			if err != nil {
				t.Fatalf("acquire temporary managed owner: %v", err)
			}
			authority := newAppAuthorityFixture()
			server := newServerWithSession(managedTailscaleFixtureConfig(root), "0.9.0", "revision", slog.New(slog.NewTextHandler(io.Discard, nil)), owner, authority)
			t.Cleanup(func() {
				if server.udp != nil {
					_ = server.udp.Close()
				}
				shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
				_ = server.hub.Shutdown(shutdownCtx)
				cancel()
				if server.webH != nil {
					_ = server.webH.Close()
				}
				if err := RetireManagedOwner(owner, nil); err != nil {
					t.Errorf("retire temporary managed owner: %v", err)
				}
			})

			if server.initErr != nil {
				t.Fatalf("managed Tailscale constructor returned an initialization error: %v", server.initErr)
			}
			if server.deviceStore() != nil || server.bootstrapGate == nil || server.bootstrapGate.OpenStatus() {
				t.Fatal("managed Tailscale constructor initialized or opened device authentication")
			}
			if _, err := server.bootstrapGate.ResolveE2EESecret(context.Background(), transport.E2EEAuthSelector{}); err == nil {
				t.Fatal("closed bootstrap gate delegated authentication without an attached store")
			}
			if _, err := server.activateTailscale(context.Background()); err == nil {
				t.Fatal("activation succeeded before local readiness")
			}
			if _, err := server.armManagedTailscale(context.Background()); err == nil {
				t.Fatal("bootstrap arm succeeded before route and local readiness")
			}
			if got := authority.activationCount(); got != 0 {
				t.Fatalf("constructor/status/arm submitted %d route activations", got)
			}
			authority.mu.Lock()
			validationCalls := authority.validationCalls
			authority.mu.Unlock()
			if validationCalls != 0 {
				t.Fatalf("constructor/status/arm unexpectedly issued %d live-owner validations", validationCalls)
			}

			if existing {
				afterBytes, err := os.ReadFile(deviceFile)
				if err != nil {
					t.Fatalf("pre-existing device file disappeared: %v", err)
				}
				dirInfo, err := os.Stat(deviceDir)
				if err != nil {
					t.Fatal(err)
				}
				fileInfo, err := os.Stat(deviceFile)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(afterBytes, beforeBytes) || dirInfo.Mode().Perm() != beforeDirMode || fileInfo.Mode().Perm() != beforeFileMode {
					t.Fatal("managed Tailscale constructor or rejected arm changed existing device-auth bytes/modes")
				}
			} else if _, err := os.Lstat(deviceDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unarmed managed constructor created device-auth path: %v", err)
			}
		})
	}
}

func TestManagedTailscaleUnresolvedRetirementKeepsOwnerInert(t *testing.T) {
	root := managedTestRoot(t)
	owner, err := AcquireManagedOwner(root)
	if err != nil {
		t.Fatal(err)
	}
	authority := newAppAuthorityFixture()
	authority.retirementErr = errors.New("injected unresolved cleanup")
	server := newServerWithSession(managedTailscaleFixtureConfig(root), "0.9.0", "revision", slog.New(slog.NewTextHandler(io.Discard, nil)), owner, authority)
	t.Cleanup(func() {
		if server.udp != nil {
			_ = server.udp.Close()
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = server.hub.Shutdown(ctx)
		cancel()
		authority.mu.Lock()
		authority.status.RouteCleared = true
		authority.status.LocalWatchClosed = true
		authority.mu.Unlock()
		if err := RetireManagedOwner(owner, nil); err != nil {
			t.Errorf("retire temporary managed owner: %v", err)
		}
	})

	if err := server.RetireManagedTailscale(context.Background()); err == nil {
		t.Fatal("unresolved route retirement was acknowledged")
	}
	if server.ManagedOwnerReleaseSafe() {
		t.Fatal("unresolved route retirement allowed owner release")
	}
	if !server.isTailscaleQuarantined() || server.bootstrapGate.OpenStatus() {
		t.Fatal("unresolved route retirement did not leave pairing revoked and backend quarantined")
	}
}

func TestTailscaleHTTPSPortUsesOnlyCanonicalOrigins(t *testing.T) {
	for _, tc := range []struct {
		origin string
		port   int
	}{
		{origin: "https://relay.example.ts.net", port: 443},
		{origin: "https://relay.example.ts.net:8443", port: 8443},
	} {
		got, err := tailscaleHTTPSPort(tc.origin)
		if err != nil || got != tc.port {
			t.Errorf("tailscaleHTTPSPort(%q) = %d, %v; want %d", tc.origin, got, err, tc.port)
		}
	}
	for _, origin := range []string{
		"http://relay.example.ts.net",
		"https://user@relay.example.ts.net",
		"https://relay.example.ts.net/",
		"https://relay.example.ts.net/path",
		"https://relay.example.ts.net:000443",
		"https://relay.example.ts.net:0",
		"https://relay.example.ts.net:65536",
	} {
		if port, err := tailscaleHTTPSPort(origin); err == nil {
			t.Errorf("tailscaleHTTPSPort(%q) = %d, accepted invalid origin", origin, port)
		}
	}
}
