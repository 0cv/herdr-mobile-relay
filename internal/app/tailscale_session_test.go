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
	"github.com/0cv/herdr-mobile-relay/internal/localcontrol"
	"github.com/0cv/herdr-mobile-relay/internal/tailscale"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

// appAuthorityFixture represents only the prepared, not-dispatched boundary
// needed by negative startup tests. It never reports an active/validated route
// or supplies ready/owned facts. Production SessionAuthority behavior and
// byte-level LocalAPI protocol cases are tested in internal/tailscale.
type appAuthorityFixture struct {
	mu                 sync.Mutex
	status             tailscale.AuthorityStatus
	origin             string
	activationCalls    int
	validationCalls    int
	retirementCalls    int
	retirementErrUntil int
	invalidation       chan struct{}
	activationErr      error
	validationErr      error
	retirementErr      error
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

func (a *appAuthorityFixture) WithValidatedRoute(ctx context.Context, admit, commit func() error) error {
	if err := a.Validate(ctx); err != nil {
		return err
	}
	if admit != nil {
		if err := admit(); err != nil {
			return err
		}
	}
	if err := a.Validate(ctx); err != nil {
		return err
	}
	return commit()
}

func (a *appAuthorityFixture) Retire(context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.retirementCalls++
	if a.retirementErr != nil && (a.retirementErrUntil == 0 || a.retirementCalls <= a.retirementErrUntil) {
		return a.retirementErr
	}
	if a.status.RegistrationOutcome == "not-dispatched" {
		a.status.RouteCleared = true
		a.status.LocalWatchClosed = true
	}
	return nil
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

func TestManagedTailscaleInitFailureUnwindsPreparedOwnerAndReleasesO(t *testing.T) {
	root := managedTestRoot(t)
	owner, err := AcquireManagedOwner(root)
	if err != nil {
		t.Fatal(err)
	}
	authority := newAppAuthorityFixture()
	server := newServerWithSession(managedTailscaleFixtureConfig(root), "0.9.0", "revision", slog.New(slog.NewTextHandler(io.Discard, nil)), owner, authority)
	server.initErr = errors.New("injected early startup failure")
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = server.hub.Shutdown(ctx)
		cancel()
		_ = server.herdrC.Close()
	}()
	if err := server.Run(context.Background()); !errors.Is(err, server.initErr) {
		t.Fatalf("Run error = %v, want injected startup failure", err)
	}
	if !server.ManagedOwnerReleaseSafe() || authority.retirementCalls != 1 {
		t.Fatalf("pre-activation failure did not prove and perform safe unwind: status=%+v retireCalls=%d", authority.Status(), authority.retirementCalls)
	}
	if err := RetireManagedOwner(owner, nil); err != nil {
		t.Fatalf("release owner after safe unwind: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "owner.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("safe startup failure retained owner lock: %v", err)
	}
}

func TestManagedTailscalePushManagerFailureUnwindsBeforeControlExists(t *testing.T) {
	root := managedTestRoot(t)
	owner, err := AcquireManagedOwner(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "push"), []byte("block push directory creation"), 0o600); err != nil {
		t.Fatal(err)
	}
	authority := newAppAuthorityFixture()
	server := newServerWithSession(managedTailscaleFixtureConfig(root), "0.9.0", "revision", slog.New(slog.NewTextHandler(io.Discard, nil)), owner, authority)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = server.hub.Shutdown(ctx)
		cancel()
		_ = server.herdrC.Close()
	}()
	if err := server.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "initialize push manager") {
		t.Fatalf("Run error = %v, want push-manager startup failure", err)
	}
	if !server.ManagedOwnerReleaseSafe() || authority.retirementCalls != 1 {
		t.Fatalf("push failure did not retire the undispatched authority: status=%+v retireCalls=%d", authority.Status(), authority.retirementCalls)
	}
	if err := RetireManagedOwner(owner, nil); err != nil {
		t.Fatalf("release owner after safe push-failure unwind: %v", err)
	}
}

func TestManagedStartupCleanupFailureRetainsOwnerUntilPrivateRetire(t *testing.T) {
	root := managedTestRoot(t)
	owner, err := AcquireManagedOwner(root)
	if err != nil {
		t.Fatal(err)
	}
	authority := newAppAuthorityFixture()
	authority.retirementErr = errors.New("first cleanup attempt unresolved")
	authority.retirementErrUntil = 1
	cfg := managedTailscaleFixtureConfig(root)
	cfg.PairingSocketPath = filepath.Join(root, "control.sock")
	server := newServerWithSession(cfg, "0.9.0", "revision", slog.New(slog.NewTextHandler(io.Discard, nil)), owner, authority)
	server.initErr = errors.New("injected pre-control startup failure")
	runDone := make(chan error, 1)
	go func() { runDone <- server.Run(context.Background()) }()
	runDoneObserved := false
	t.Cleanup(func() {
		if !server.ManagedOwnerReleaseSafe() {
			authority.mu.Lock()
			authority.status.RouteCleared = true
			authority.status.LocalWatchClosed = true
			authority.mu.Unlock()
			server.CompleteManagedTailscaleRetirement()
		}
		if !runDoneObserved {
			select {
			case <-runDone:
			case <-time.After(time.Second):
				t.Error("startup cleanup did not leave its owner-retaining wait")
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = server.hub.Shutdown(ctx)
		cancel()
		_ = server.herdrC.Close()
		if server.ManagedOwnerReleaseSafe() {
			if err := RetireManagedOwner(owner, nil); err != nil {
				t.Errorf("release owner after startup cleanup: %v", err)
			}
		}
	})

	controlPath := cfg.PairingSocketPath
	var status localcontrol.Response
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		status, err = localcontrol.Request(context.Background(), controlPath, "status", cfg.ManagedRunID, cfg.InstanceID)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil || !status.OwnerHeld || status.RouteCleared {
		t.Fatalf("startup cleanup did not retain O behind a live retirement-only control: status=%+v err=%v", status, err)
	}
	if _, err := os.Stat(filepath.Join(root, "owner.lock")); err != nil {
		t.Fatalf("owner lock disappeared before cleanup proof: %v", err)
	}
	retired, err := localcontrol.Request(context.Background(), controlPath, "retire", cfg.ManagedRunID, cfg.InstanceID)
	if err != nil || !retired.RouteCleared || !retired.LocalWatchClosed {
		t.Fatalf("private retry retirement = %+v, err=%v", retired, err)
	}
	select {
	case runErr := <-runDone:
		runDoneObserved = true
		if !errors.Is(runErr, server.initErr) {
			t.Fatalf("Run error = %v, want original startup failure", runErr)
		}
	case <-time.After(time.Second):
		t.Fatal("safe private retirement did not release the startup wait")
	}
	if !server.ManagedOwnerReleaseSafe() {
		t.Fatal("owner release was not enabled after private retirement proof")
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
