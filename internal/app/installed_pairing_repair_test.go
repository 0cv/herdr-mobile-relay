//go:build herdr_installed_inert_test

package app

import (
	"context"
	"crypto/tls"
	"errors"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/deviceauth"
	"github.com/0cv/herdr-mobile-relay/internal/localcontrol"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

// Pure lower-level phase checks: no positive concrete qualification, runtime
// construction, network, file, process or readiness bypass.
func TestInstalledPairingSyntheticAdmissionPhases(t *testing.T) {
	for _, name := range []string{"nil-nonready", "late-local-loss", "terminal", "uncertainty"} {
		t.Run(name, func(t *testing.T) {
			s := &Server{cfg: &config.Config{Transport: config.TransportTailscaleCLI}, bootstrapGate: deviceauth.NewBootstrapGate(), installedCLI: &tailscalecli.InstalledSession{}}
			status := localcontrol.Status{ArmOutcome: "committed"}
			var failure error
			switch name {
			case "late-local-loss":
				status.PersistentRouteReadiness = string(tailscalecli.ReadinessReady)
			case "terminal":
				failure = errors.New("synthetic bundle failure")
			case "uncertainty":
				failure = deviceauth.ErrManagedArmRecovery
			}
			if err := s.installedPairingAdmission(status, failure); err == nil {
				t.Fatal("nonready accepted")
			}
			terminal := name == "terminal" || name == "uncertainty"
			if s.installedAdmissionRevoked() != terminal {
				t.Fatal("incorrect latch")
			}
			if status.ArmOutcome != "committed" {
				t.Fatal("commit metadata lost")
			}
			if terminal {
				s.recordInstalledPairingFailure(nil)
				if !s.installedAdmissionRevoked() {
					t.Fatal("healthy observation reopened terminal state")
				}
				if p, _, err := s.reprintInstalledPairing(context.Background(), config.InstalledConfig{}); err == nil || p.URL != "" {
					t.Fatal("later request accepted")
				}
			} else if !s.quarantined {
				t.Fatal("availability not quarantined")
			}
		})
	}
}

func TestInstalledPairingSyntheticErrorTrees(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		terminal bool
	}{
		{"cancel", context.Canceled, false},
		{"availability", tailscalecli.ErrTransientUnavailable, false},
		{"cancel-availability", errors.Join(context.Canceled, tailscalecli.ErrTransientUnavailable), false},
		{"missing", deviceauth.ErrBootstrapUnavailable, false},
		{"expired", deviceauth.ErrInvitationExpired, false},
		{"burned", deviceauth.ErrInvitationBurned, false},
		{"pending", deviceauth.ErrBootstrapUnavailable, false},
		{"consumed", deviceauth.ErrBootstrapUnavailable, false},
		{"uncertain", deviceauth.ErrManagedArmRecovery, true},
		{"cancel-conflict", errors.Join(context.Canceled, tailscalecli.ErrConflict), true},
		{"cancel-unknown", errors.Join(context.Canceled, errors.New("synthetic unknown")), true},
		{"certificate", &tls.CertificateVerificationError{Err: context.Canceled}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{installedCLI: &tailscalecli.InstalledSession{}, bootstrapGate: deviceauth.NewBootstrapGate()}
			s.recordInstalledPairingFailure(tc.err)
			if s.installedAdmissionRevoked() != tc.terminal {
				t.Fatal("error tree precedence")
			}
		})
	}
}

func TestInstalledPairingSyntheticRetiredResultBoundary(t *testing.T) {
	// An active backend cannot substitute for a live owner.
	b := newInstalledBackend(context.Background(), &installedLogicalListener{})
	defer b.Close()
	s := &Server{installedBackend: b, installedCLI: &tailscalecli.InstalledSession{}}
	s.tailscaleCLIRegistration = s.installedCLI
	if err := s.installedPairingResultBoundary(context.Background(), config.InstalledConfig{}); err == nil {
		t.Fatal("retired owner accepted")
	}
	// Cancel actual lifetime without waiting for AfterFunc propagation.
	b.cancel()
	if err := s.installedPairingResultBoundary(context.Background(), config.InstalledConfig{}); err == nil {
		t.Fatal("retired backend accepted")
	}
}
