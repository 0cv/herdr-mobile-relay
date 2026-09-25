package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/config"
)

func TestTailscaleExternalConstructionIsDeferredAndUnowned(t *testing.T) {
	runtimeDir := t.TempDir()
	socket := filepath.Join(runtimeDir, "external-control.sock")
	cfg := &config.Config{
		Transport:           config.TransportTailscaleExternal,
		RuntimeDir:          runtimeDir,
		ConfigHome:          filepath.Join(runtimeDir, "config"),
		ExternalHTTPSOrigin: "https://relay.example.test",
		PhoneAppOrigin:      "https://app.example.test",
		ControlRunID:        "external-control-run",
		PairingSocketPath:   socket,
		Token:               strings.Repeat("k", 32),
		Host:                "127.0.0.1",
		CacheDir:            "",
		TailscaleBin:        filepath.Join(runtimeDir, "must-not-run"),
	}

	server, err := NewOwned(cfg, "1.2.3", strings.Repeat("a", 40), managedTestLogger(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if server.managedOwner != nil || server.tailscaleSession != nil {
		t.Fatal("operator-owned transport acquired managed Tailscale route ownership")
	}
	if server.deviceStore() == nil || server.bootstrapGate == nil || server.bootstrapGate.OpenStatus() {
		t.Fatal("external bootstrap gate/store was not attached in its closed, deferred state")
	}
	if status := server.externalControlStatus(); status.PhoneAppOrigin != cfg.PhoneAppOrigin {
		t.Fatalf("external control phone-app origin = %q, want %q", status.PhoneAppOrigin, cfg.PhoneAppOrigin)
	}
	for _, path := range []string{
		filepath.Join(runtimeDir, "device-auth"),
		filepath.Join(runtimeDir, "owner.lock"),
		socket,
		cfg.TailscaleBin,
	} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("construction created or inspected a managed/external resource at %q: %v", path, err)
		}
	}
}

func TestTailscaleExternalRefusesManagedOwner(t *testing.T) {
	cfg := &config.Config{
		Transport:           config.TransportTailscaleExternal,
		RuntimeDir:          t.TempDir(),
		ExternalHTTPSOrigin: "https://relay.example.test",
		PhoneAppOrigin:      "https://app.example.test",
		ControlRunID:        "external-control-run",
		PairingSocketPath:   filepath.Join(t.TempDir(), "external-control.sock"),
		ManagedRunID:        "managed-run",
	}
	if _, err := NewOwned(cfg, "1.2.3", strings.Repeat("a", 40), managedTestLogger(), nil); err == nil {
		t.Fatal("operator-owned HTTPS Serve accepted managed Tailscale ownership")
	}
}
