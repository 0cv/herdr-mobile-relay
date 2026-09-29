package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

func TestTailscaleCLIConstructionFailsBeforeOpeningResources(t *testing.T) {
	runtimeDir := t.TempDir()
	cfg := &config.Config{
		Transport:    config.TransportTailscaleCLI,
		RuntimeDir:   runtimeDir,
		TailscaleBin: filepath.Join(runtimeDir, "must-not-run"),
	}
	if _, err := NewOwned(cfg, "1.2.3", strings.Repeat("a", 40), managedTestLogger(), nil); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("unqualified CLI transport construction was not refused: %v", err)
	}
	for _, path := range []string{
		filepath.Join(runtimeDir, "device-auth"),
		filepath.Join(runtimeDir, "owner.lock"),
		cfg.TailscaleBin,
	} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("refused construction touched %q: %v", path, err)
		}
	}
}

type fixedCLIRouteVerifier struct {
	status tailscalecli.RouteStatus
	err    error
	calls  int
}

func (v *fixedCLIRouteVerifier) VerifyRegisteredRoute(context.Context, string, string, string, int, int) (tailscalecli.RouteStatus, error) {
	v.calls++
	return v.status, v.err
}

func TestCLITailscaleControlStatusFailsClosedOnUnqualifiedRoute(t *testing.T) {
	runtimeDir := t.TempDir()
	cfg := &config.Config{
		Transport:          config.TransportTailscaleCLI,
		RuntimeDir:         runtimeDir,
		ConfigHome:         runtimeDir,
		TailscaleCLIOrigin: "https://fixture.example.test",
		TailscaleCLIScope:  "development",
		InstanceID:         "fixture-instance",
		ControlRunID:       "fixture-control",
		PairingSocketPath:  filepath.Join(runtimeDir, "control.sock"),
		PhoneAppOrigin:     "https://fixture-app.example.test",
		Host:               "127.0.0.1",
		Port:               18377,
		Token:              strings.Repeat("k", 32),
	}
	server := newServerWithSession(cfg, "1.2.3", strings.Repeat("a", 40), managedTestLogger(), nil, nil)
	verifier := &fixedCLIRouteVerifier{status: tailscalecli.RouteStatus{
		JournalState: tailscalecli.StateRegistered,
		Readiness:    tailscalecli.ReadinessReady,
		// Source/fixture observation is never P6 live-runtime qualification.
		RuntimeQualified: false,
	}}
	server.tailscaleCLIRegistration = verifier

	status := server.tailscaleCLIControlStatus(context.Background())
	if status.PersistentRouteState != string(tailscalecli.StateRegistered) ||
		status.PersistentRouteReadiness != string(tailscalecli.ReadinessReady) ||
		status.PersistentRouteReady || status.ServeReady || status.Ready || !status.Quarantined {
		t.Fatalf("unqualified CLI route status admitted control: %+v", status)
	}
	if server.bootstrapGate.OpenStatus() {
		t.Fatal("unqualified CLI route reopened bootstrap admission")
	}

	armStatus, err := server.armTailscaleCLI(context.Background())
	if err == nil || armStatus.ArmOutcome != "not-committed" || armStatus.InvitationArmed || armStatus.Ready {
		t.Fatalf("CLI arm without local readiness = %+v, %v", armStatus, err)
	}
	if server.bootstrapGate.OpenStatus() || verifier.calls < 2 {
		t.Fatalf("CLI arm changed admission or skipped read-only route checks: open=%v checks=%d", server.bootstrapGate.OpenStatus(), verifier.calls)
	}
}

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

func TestExternalArmFailureCodeIsSafeAndSpecific(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{name: "local readiness", err: errors.New("local relay inventory or backend readiness is incomplete"), want: "local_readiness_incomplete"},
		{name: "phone bundle", err: errors.New("external phone app bundle verification failed: https://private.example.test/token"), want: "phone_app_bundle_mismatch"},
		{name: "unknown detail is not exposed", err: errors.New("private origin https://private.example.test/token"), want: "bootstrap_invitation_refused"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := externalArmFailureCode(test.err); got != test.want {
				t.Fatalf("failure code = %q, want %q", got, test.want)
			}
		})
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
