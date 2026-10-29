package app

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/deviceauth"
	"github.com/0cv/herdr-mobile-relay/internal/localcontrol"
	"github.com/0cv/herdr-mobile-relay/internal/setuphelper"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

type installedPairingPayload struct{ URL string }

func (installedPairingPayload) String() string     { return "installed pairing (redacted)" }
func (p installedPairingPayload) GoString() string { return p.String() }

func installedInvitationPayload(inv deviceauth.BootstrapInvitationSnapshot, origin, phone, label string) (installedPairingPayload, error) {
	raw, err := base64.RawURLEncoding.DecodeString(inv.Secret)
	if err != nil || len(raw) != 32 || inv.InvitationID != "bootstrap" || inv.Version != 1 || inv.Pending || !utf8.Valid(raw) {
		return installedPairingPayload{}, deviceauth.ErrBootstrapUnavailable
	}
	token := string(raw)
	// The existing bootstrap client UTF-8 encodes a raw token, not base64.
	// Restrict to printable ASCII so JS length and UTF-8 round trips are exact.
	for _, c := range token {
		if c > 126 || unicode.IsControl(c) {
			return installedPairingPayload{}, deviceauth.ErrBootstrapUnavailable
		}
	}
	if _, err := setuphelper.NormalizeExternalHTTPSOrigin(origin); err != nil {
		return installedPairingPayload{}, deviceauth.ErrBootstrapUnavailable
	}
	if _, err := setuphelper.NormalizeExternalHTTPSOrigin(phone); err != nil {
		return installedPairingPayload{}, deviceauth.ErrBootstrapUnavailable
	}
	return installedPairingPayload{URL: strings.TrimSuffix(phone, "/") + "/#" + setuphelper.SetupFragment(token, label, "wss://"+strings.TrimPrefix(origin, "https://"))}, nil
}

func (s *Server) armInstalledPairing(ctx context.Context, selected config.InstalledConfig) (installedPairingPayload, localcontrol.Status, error) {
	return s.installedPairing(ctx, selected, true)
}
func (s *Server) reprintInstalledPairing(ctx context.Context, selected config.InstalledConfig) (installedPairingPayload, localcontrol.Status, error) {
	return s.installedPairing(ctx, selected, false)
}

// Private leaf only: future authenticated composition supplies its ORIGINAL
// selection. No path rebinding, new writer, public lease or readiness assertion.
func (s *Server) installedPairing(ctx context.Context, selected config.InstalledConfig, arm bool) (payload installedPairingPayload, status localcontrol.Status, err error) {
	status.ArmOutcome = "not-committed"
	if s == nil || ctx == nil || s.installedAdmissionRevoked() || s.deviceStore() == nil || s.bootstrapGate == nil {
		return payload, status, tailscalecli.ErrWorkflowRequired
	}
	if err = selected.Validate(s.cfg); err != nil {
		return
	}
	operation := func(call context.Context) error {
		return s.withInstalledPairingScope(call, func(joined context.Context) error {
			if s.installedAdmissionRevoked() {
				return tailscalecli.ErrWorkflowRequired
			}
			store := s.deviceStore()
			if arm {
				status, err = s.armTailscaleCLILocked(joined)
			} else {
				status, err = s.admitTailscaleCLILocked(joined)
				status.ArmOutcome = "not-committed"
			}
			err = s.installedPairingAdmission(status, err)
			if err != nil {
				return err
			}
			inv, err := s.bootstrapGate.CurrentBootstrapInvitation(joined)
			if err != nil {
				s.recordInstalledPairingFailure(err)
				return err
			}
			result, err := installedInvitationPayload(inv, s.cfg.TailscaleCLIOrigin, s.cfg.PhoneAppOrigin, s.hostname)
			if err != nil {
				return err
			}
			if err := s.installedPairingResultBoundary(joined, selected); err != nil {
				return err
			}
			// Binding validation above can block on filesystem reads. Check the
			// mutable guards afterward, immediately before releasing the payload.
			payload, err = s.installedPairingRelease(store, result)
			return err
		})
	}
	if a := s.installedSetup; a != nil {
		err = (InstalledSetupControl{state: a}).operation(ctx, func(call context.Context, owned *installedSetupState) error {
			owned.mu.Lock()
			matches := owned.backend != nil && owned.backend == s.installedBackend && owned.binding == selected.Binding()
			owned.mu.Unlock()
			if !matches || !owned.registered() || s.tailscaleCLIRegistration != owned {
				return tailscalecli.ErrWorkflowRequired
			}
			if err := owned.session.ValidateRuntimeBindingContext(call, selected.Binding()); err != nil {
				return err
			}
			return operation(call)
		})
	} else if s.installedCLI != nil && s.tailscaleCLIRegistration == s.installedCLI {
		if err = s.installedCLI.ValidateRuntimeBinding(selected.Binding()); err == nil {
			err = operation(ctx)
		}
	} else {
		err = tailscalecli.ErrWorkflowRequired
	}
	if err != nil {
		payload = installedPairingPayload{}
		status.Ready = false
		status.ArmFailureCode = externalArmFailureCode(err)
		if errors.Is(err, deviceauth.ErrManagedArmRecovery) {
			status.ArmOutcome = "unresolved"
		}
	}
	return
}
