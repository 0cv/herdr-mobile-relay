package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/appdeploy"
	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/deviceauth"
	"github.com/0cv/herdr-mobile-relay/internal/localcontrol"
	"github.com/0cv/herdr-mobile-relay/internal/tailscale"
)

const managedHealthTimeout = 5 * time.Second

type managedTailscaleAuthority interface {
	Activate(context.Context) error
	Validate(context.Context) error
	WithValidatedRoute(context.Context, func() error, func() error) error
	Retire(context.Context) error
	Invalidation() <-chan struct{}
	Status() tailscale.AuthorityStatus
	Origin() (string, bool)
}

// prepareManagedTailscale constructs the real in-process LocalAPI owner only
// after O has been validated by NewOwned. CLI inspection remains bounded and
// read-only; neither its route observation nor environment values authorize a
// Serve mutation.
func prepareManagedTailscale(cfg *config.Config) (*tailscale.SessionAuthority, error) {
	port, err := tailscaleHTTPSPort(cfg.TailscaleOrigin)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tailscaleBin := cfg.TailscaleBin
	if tailscaleBin == "" {
		tailscaleBin = "tailscale"
	}
	inspection, err := tailscale.Inspect(ctx, tailscaleBin, port)
	if err != nil {
		return nil, fmt.Errorf("read-only Tailscale inspection failed: %w", err)
	}
	if !inspection.LoggedIn || !inspection.ServeInspected || !inspection.ExposureComplete ||
		inspection.ServeConfigured || inspection.FunnelConfigured || inspection.ServeRouteCount != 0 ||
		len(inspection.ServeRoutes) != 0 {
		return nil, errors.New("Tailscale is not in the supported authenticated, empty-Serve state")
	}
	if inspection.Origin != cfg.TailscaleOrigin || inspection.StatusVersion == "" || len(inspection.VersionMetadata) == 0 {
		return nil, errors.New("Tailscale identity, version metadata, or configured origin does not match")
	}
	authority, err := tailscale.NewSessionAuthority(inspection.StatusVersion, inspection.VersionMetadata, port, cfg.Port)
	if err != nil {
		return nil, fmt.Errorf("construct Tailscale session authority: %w", err)
	}
	if err := authority.Prepare(ctx); err != nil {
		return nil, fmt.Errorf("read-only Tailscale owner preparation failed: %w", err)
	}
	preparedOrigin, ok := authority.Origin()
	if !ok || preparedOrigin != cfg.TailscaleOrigin {
		return nil, errors.New("prepared Tailscale node origin does not match configured origin")
	}
	return authority, nil
}

func tailscaleHTTPSPort(origin string) (int, error) {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return 0, errors.New("configured Tailscale origin is not a canonical HTTPS origin")
	}
	if parsed.Port() == "" {
		return tailscale.DefaultHTTPSPort, nil
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1 || port > 65535 || strconv.Itoa(port) != parsed.Port() {
		return 0, errors.New("configured Tailscale HTTPS port is invalid")
	}
	return port, nil
}

func (s *Server) managedLocalReady() bool {
	if s == nil || s.state == nil || s.udp == nil {
		return false
	}
	s.mu.RLock()
	bound := s.backendBound
	s.mu.RUnlock()
	return bound && s.state.InventoryReady()
}

func (s *Server) validateTailscaleOwner(ctx context.Context) error {
	if s == nil || s.tailscaleSession == nil {
		return errors.New("managed Tailscale owner is unavailable")
	}
	if err := s.validateManagedOwner(); err != nil {
		return err
	}
	if err := s.tailscaleSession.Validate(ctx); err != nil {
		return fmt.Errorf("live Tailscale route validation failed: %w", err)
	}
	return nil
}

func (s *Server) validateManagedOwner() error {
	if s == nil || s.managedOwner == nil {
		return errors.New("managed owner is unavailable")
	}
	if err := s.managedOwner.Validate(); err != nil {
		return fmt.Errorf("managed owner validation failed: %w", err)
	}
	return nil
}

func (s *Server) activateTailscale(ctx context.Context) (localcontrol.Status, error) {
	unlock, err := s.tailscaleOpMu.Lock(ctx)
	if err != nil {
		return s.pairingControlStatusContext(ctx), err
	}
	defer unlock()
	if !s.managedLocalReady() {
		return s.pairingControlStatusContext(ctx), errors.New("local relay inventory, UDP, or backend readiness is incomplete")
	}
	if s.managedOwner == nil {
		return s.pairingControlStatusContext(ctx), errors.New("managed owner is unavailable for Tailscale activation")
	}
	if err := s.managedOwner.Validate(); err != nil {
		return s.pairingControlStatusContext(ctx), err
	}
	state := s.tailscaleSession.Status()
	if state.Active && state.RouteValidated && !state.Invalidated && !state.Quarantined {
		if err := s.tailscaleSession.Validate(ctx); err != nil {
			s.quarantineTailscale()
			return s.pairingControlStatusContext(ctx), err
		}
	} else {
		if state.Invalidated || state.Quarantined || state.RouteCleared || state.RegistrationOutcome != "not-dispatched" {
			s.quarantineTailscale()
			return s.pairingControlStatusContext(ctx), errors.New("Tailscale activation is no longer admissible")
		}
		if err := s.tailscaleSession.Activate(ctx); err != nil {
			s.quarantineTailscale()
			return s.pairingControlStatusContext(ctx), err
		}
	}
	if err := s.bindManagedAdmissionWatch(); err != nil {
		s.quarantineTailscale()
		return s.pairingControlStatusContext(ctx), err
	}
	if err := s.checkManagedTailscaleReadiness(ctx, true); err != nil {
		s.quarantineTailscale()
		return s.pairingControlStatusContext(ctx), err
	}
	return s.pairingControlStatusContext(ctx), nil
}

func (s *Server) bindManagedAdmissionWatch() error {
	authority, ok := s.tailscaleSession.(interface {
		AdmissionChannels() (<-chan struct{}, <-chan struct{})
	})
	if !ok || s.bootstrapGate == nil {
		return errors.New("managed Tailscale authority has no live admission guard")
	}
	watchEnded, invalidated := authority.AdmissionChannels()
	if err := s.bootstrapGate.BindAuthorityAdmission(watchEnded, invalidated); err != nil {
		return fmt.Errorf("bind live Tailscale admission guard: %w", err)
	}
	return nil
}

func (s *Server) armManagedTailscale(ctx context.Context) (localcontrol.Status, error) {
	refused := func(outcome string, err error) (localcontrol.Status, error) {
		status := s.pairingControlStatusContext(ctx)
		status.ArmOutcome = outcome
		return status, err
	}
	unlock, err := s.tailscaleOpMu.Lock(ctx)
	if err != nil {
		return refused("not-committed", err)
	}
	defer unlock()
	if err := s.checkManagedTailscaleReadiness(ctx, true); err != nil {
		s.quarantineTailscale()
		return refused("not-committed", err)
	}
	if err := s.ensureManagedDeviceStore(); err != nil {
		return refused("not-committed", err)
	}

	// Pairing revocation, the final owner validation, durable arm and Hub
	// admission handoff are serialized together. The authority operation lock
	// remains held through the transition; the gate independently checks raw
	// watch EOF and invalidation channels without taking authority locks.
	s.pairingAdmissionMu.Lock()
	defer s.pairingAdmissionMu.Unlock()
	var invitation deviceauth.BootstrapStatus
	armOutcome := "not-committed"
	armCommitted := false
	err = s.tailscaleSession.WithValidatedRoute(ctx, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.checkManagedTailscaleReadinessWithoutOwner(ctx, true); err != nil {
			return err
		}
		return s.validateManagedOwner()
	}, func() error {
		unlockAdmission, err := s.admissionTransitionMu.Lock(ctx)
		if err != nil {
			return err
		}
		defer unlockAdmission()
		if err := s.bootstrapGate.ArmBootstrapInvitation([]byte(s.cfg.Token), s.hostname, "en", func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return s.validateManagedOwner()
		}, func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := s.validateManagedOwner(); err != nil {
				return err
			}
			if s.managedAdmissionHandoffObserver != nil {
				s.managedAdmissionHandoffObserver()
			}
			return s.hub.SetAcceptingContext(ctx, true)
		}); err != nil {
			if errors.Is(err, deviceauth.ErrManagedArmRecovery) {
				armOutcome = "unresolved"
			} else if errors.Is(err, deviceauth.ErrBootstrapGateCommittedRevoked) {
				armOutcome = "committed"
			}
			return err
		}
		armCommitted = true
		armOutcome = "committed"
		invitation = s.deviceStore().BootstrapStatus()
		s.mu.Lock()
		s.quarantined = false
		s.mu.Unlock()
		return nil
	})
	if err != nil {
		if armCommitted {
			armOutcome = "committed"
		}
		if errors.Is(err, deviceauth.ErrManagedArmRecovery) || s.tailscaleSession.Status().Invalidated || s.tailscaleSession.Status().Quarantined || errors.Is(err, deviceauth.ErrBootstrapGateClosed) {
			s.quarantineTailscale()
		}
		s.recordSafeError("bootstrap invitation arm failed", err)
		return refused(armOutcome, err)
	}

	// The durable invitation was snapshotted inside the authority-locked
	// commit before the Hub handoff. A phone may consume it immediately after
	// that transition; the control acknowledgement still reports the commit.
	ownerStatus := s.tailscaleSession.Status()
	serveReady := ownerStatus.Active && ownerStatus.RouteValidated && !ownerStatus.Quarantined && !ownerStatus.Invalidated && !ownerStatus.RouteCleared
	status := localcontrol.Status{
		Ready:                        serveReady && invitation.Armed,
		OwnerHeld:                    true,
		LocalReady:                   true,
		ServeReady:                   serveReady,
		Quarantined:                  ownerStatus.Quarantined || ownerStatus.Invalidated,
		RouteCleared:                 ownerStatus.RouteCleared,
		LocalWatchClosed:             ownerStatus.LocalWatchClosed,
		RemoteWatchRetirementUnknown: ownerStatus.RemoteWatchRetirementUnknown,
		Transport:                    s.cfg.Transport,
		Version:                      s.version,
		Revision:                     s.revision,
		InvitationArmed:              invitation.Armed,
		InvitationPending:            invitation.Pending,
		ArmOutcome:                   "committed",
	}
	if s.webH != nil {
		status.BundleHash = s.webH.BundleHash()
	}
	if !invitation.ExpiresAt.IsZero() {
		status.InvitationExpiresAt = invitation.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if status.Quarantined || !status.ServeReady || !s.bootstrapGate.OpenStatus() {
		s.quarantineTailscaleLocked()
		return status, errors.New("Tailscale admission was revoked after the durable invitation commit")
	}
	return status, nil
}

// armExternalTailscale keeps operator-owned Serve separate from managed
// SessionAuthority: it never probes Tailscale CLI/LocalAPI and never claims or
// changes ingress. The relay HTTPS endpoint and selected phone-app bundle are
// verified with normal system TLS immediately before the durable invitation
// transaction and again at its commit boundary.
func (s *Server) armExternalTailscale(ctx context.Context) (localcontrol.Status, error) {
	refused := func(err error) (localcontrol.Status, error) {
		status := s.externalControlStatus()
		status.ArmOutcome = "not-committed"
		status.ArmFailureCode = externalArmFailureCode(err)
		if errors.Is(err, deviceauth.ErrManagedArmRecovery) {
			status.ArmOutcome = "unresolved"
		} else if errors.Is(err, deviceauth.ErrBootstrapGateCommittedRevoked) {
			status.ArmOutcome = "committed"
		}
		return status, err
	}
	unlock, err := s.externalArmMu.Lock(ctx)
	if err != nil {
		return refused(err)
	}
	defer unlock()
	if err := s.checkExternalTailscaleReadiness(ctx); err != nil {
		return refused(err)
	}
	if err := s.ensureManagedDeviceStore(); err != nil {
		return refused(err)
	}

	s.pairingAdmissionMu.Lock()
	defer s.pairingAdmissionMu.Unlock()
	if err := s.bootstrapGate.ArmBootstrapInvitation([]byte(s.cfg.Token), s.hostname, "en", nil, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.checkExternalTailscaleReadiness(ctx); err != nil {
			return err
		}
		return s.hub.SetAcceptingContext(ctx, true)
	}); err != nil {
		s.recordSafeError("operator-owned Serve bootstrap arm failed", err)
		return refused(err)
	}
	status := s.externalControlStatus()
	status.Ready = status.LocalReady && s.bootstrapGate.OpenStatus()
	status.InvitationArmed = true
	status.ArmOutcome = "committed"
	if store := s.deviceStore(); store != nil {
		invitation := store.BootstrapStatus()
		status.InvitationPending = invitation.Pending
		if !invitation.ExpiresAt.IsZero() {
			status.InvitationExpiresAt = invitation.ExpiresAt.UTC().Format(time.RFC3339)
		}
	}
	if !status.Ready {
		status.ArmFailureCode = "local_admission_unavailable"
		return status, errors.New("operator-owned Serve invitation was committed but local admission is unavailable")
	}
	return status, nil
}

func externalArmFailureCode(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, deviceauth.ErrManagedArmRecovery):
		return "bootstrap_recovery_required"
	case errors.Is(err, deviceauth.ErrBootstrapGateCommittedRevoked):
		return "bootstrap_committed_revoked"
	case errors.Is(err, deviceauth.ErrBootstrapGateClosed):
		return "bootstrap_gate_closed"
	case errors.Is(err, context.DeadlineExceeded):
		return "external_operation_timeout"
	case errors.Is(err, context.Canceled):
		return "external_operation_cancelled"
	}
	message := err.Error()
	switch {
	case message == "local relay inventory or backend readiness is incomplete":
		return "local_readiness_incomplete"
	case message == "local web bundle identity does not match the relay binary":
		return "local_bundle_identity_mismatch"
	case strings.HasPrefix(message, "local relay health check failed:"):
		return "local_health_check_failed"
	case strings.HasPrefix(message, "trusted external HTTPS health check failed:"):
		return "external_https_unavailable"
	case message == "external HTTPS health endpoint identity did not match this relay":
		return "external_https_endpoint_identity_mismatch"
	case message == "external HTTPS health response was invalid":
		return "external_https_health_invalid"
	case message == "external HTTPS relay, release, or web-bundle identity did not match":
		return "external_https_release_identity_mismatch"
	case message == "verified external phone app origin is unavailable":
		return "phone_app_origin_unavailable"
	case strings.HasPrefix(message, "external phone app bundle verification failed:"):
		return "phone_app_bundle_mismatch"
	case strings.HasPrefix(message, "open device authentication"):
		return "device_store_unavailable"
	case message == "operator-owned Serve invitation was committed but local admission is unavailable":
		return "local_admission_unavailable"
	default:
		return "bootstrap_invitation_refused"
	}
}

func (s *Server) checkExternalTailscaleReadiness(ctx context.Context) error {
	if !s.managedLocalReady() {
		return errors.New("local relay inventory or backend readiness is incomplete")
	}
	if s.webH == nil || s.webH.BundleVersion() != s.version || s.webH.BundleRevision() != s.revision {
		return errors.New("local web bundle identity does not match the relay binary")
	}
	if err := s.checkLocalHealth(ctx); err != nil {
		return err
	}
	origin := s.cfg.ExternalHTTPSOrigin
	checkCtx, cancel := context.WithTimeout(ctx, managedHealthTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(checkCtx, http.MethodGet, strings.TrimSuffix(origin, "/")+"/healthz", nil)
	if err != nil {
		return err
	}
	client := managedHealthClientForServer(s, managedHealthTimeout)
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("trusted external HTTPS health check failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("X-Herdr-Relay-Instance") != s.cfg.InstanceID {
		return errors.New("external HTTPS health endpoint identity did not match this relay")
	}
	var health struct {
		Status         string `json:"status"`
		Readiness      string `json:"readiness"`
		Transport      string `json:"transport"`
		Instance       string `json:"instance"`
		ControlRunID   string `json:"external_control_run_id"`
		Version        string `json:"version"`
		Revision       string `json:"revision"`
		Origin         string `json:"external_https_origin"`
		BundleHash     string `json:"bundle_hash"`
		BundleVersion  string `json:"bundle_version"`
		BundleRevision string `json:"bundle_revision"`
	}
	if err := decodeManagedHealth(response.Body, &health); err != nil {
		return errors.New("external HTTPS health response was invalid")
	}
	if health.Status != "ok" || health.Readiness != "ready" || health.Transport != config.TransportTailscaleExternal ||
		health.Instance != s.cfg.InstanceID || health.ControlRunID != s.cfg.ControlRunID ||
		health.Version != s.version || health.Revision != s.revision || health.Origin != origin ||
		s.webH == nil || health.BundleHash != s.webH.BundleHash() ||
		health.BundleVersion != s.version || health.BundleRevision != s.revision {
		return errors.New("external HTTPS relay, release, or web-bundle identity did not match")
	}
	if s.cfg.PhoneAppOrigin == "" {
		return errors.New("verified external phone app origin is unavailable")
	}
	if err := appdeploy.VerifyPublic(ctx, s.cfg.WebRoot, s.cfg.PhoneAppOrigin, s.version, s.revision); err != nil {
		return fmt.Errorf("external phone app bundle verification failed: %w", err)
	}
	return nil
}

// unwindManagedStartup is used only before the private control socket is
// published. In that phase activation is provably undispatched, so the real
// SessionAuthority can safely retire its prepared state without a route POST.
// An unexpected dispatched/active state is not silently released.
func (s *Server) unwindManagedStartup(startupErr error) error {
	if s == nil || s.tailscaleSession == nil {
		return startupErr
	}
	state := s.tailscaleSession.Status()
	if state.RouteCleared && state.LocalWatchClosed {
		s.CompleteManagedTailscaleRetirement()
		return startupErr
	}
	if state.Active || state.RegistrationOutcome != "not-dispatched" {
		return s.retainManagedOwnerForStartupFailure(startupErr, errors.New("startup failure occurred after Tailscale activation may have been dispatched"))
	}
	retireCtx, cancel := context.WithTimeout(context.Background(), localcontrol.RetireTimeout)
	retireErr := s.RetireManagedTailscale(retireCtx)
	cancel()
	if retireErr != nil || !s.ManagedOwnerReleaseSafe() {
		if retireErr == nil {
			retireErr = errors.New("pre-activation retirement did not prove route clear and local watch closure")
		}
		return s.retainManagedOwnerForStartupFailure(startupErr, fmt.Errorf("safe pre-activation Tailscale unwind failed: %w", retireErr))
	}
	s.CompleteManagedTailscaleRetirement()
	return startupErr
}

// retainManagedOwnerForStartupFailure prevents a cleanup failure before the
// normal control server exists from returning through runServe and dropping O.
// It publishes a retirement-only control endpoint when possible and blocks
// until the real SessionAuthority proves route clear and local watch closure.
func (s *Server) retainManagedOwnerForStartupFailure(startupErr, unwindErr error) error {
	s.quarantineTailscale()
	combinedErr := errors.Join(startupErr, unwindErr)
	if s.cfg.PairingSocketPath == "" {
		s.logger.Error("startup cleanup is unresolved and no private control socket is configured; retaining managed owner", "error", combinedErr)
		<-s.managedRetired
		return combinedErr
	}
	control, err := localcontrol.NewManaged(s.cfg.PairingSocketPath, s.cfg.ManagedRunID, s.cfg.InstanceID, localcontrol.Callbacks{
		Status: s.pairingControlStatusContext,
		Activate: func(ctx context.Context) (localcontrol.Status, error) {
			return s.controlStatus(ctx), errors.New("startup failed; Tailscale activation is unavailable")
		},
		Arm: func(ctx context.Context) (localcontrol.Status, error) {
			return s.controlStatus(ctx), errors.New("startup failed; bootstrap arming is unavailable")
		},
		Retire:  s.retireForControl,
		Retired: s.CompleteManagedTailscaleRetirement,
	})
	if err != nil {
		s.logger.Error("startup cleanup is unresolved and retirement control could not be published; retaining managed owner", "error", errors.Join(combinedErr, err))
		<-s.managedRetired
		return errors.Join(combinedErr, err)
	}
	s.pairingControl = control
	controlCtx, cancel := context.WithCancel(context.Background())
	controlDone := make(chan error, 1)
	go func() { controlDone <- control.Run(controlCtx) }()
	select {
	case <-s.managedRetired:
	case controlErr := <-controlDone:
		s.logger.Error("startup retirement control stopped while cleanup is unresolved; retaining managed owner", "error", controlErr)
		<-s.managedRetired
	}
	cancel()
	if err := control.Close(); err != nil {
		s.logger.Error("close startup retirement control", "error", err)
		combinedErr = errors.Join(combinedErr, err)
	}
	s.pairingControl = nil
	return combinedErr
}

func (s *Server) ensureManagedDeviceStore() error {
	store := s.deviceStore()
	if store == nil {
		var err error
		store, err = deviceauth.OpenDeferred(filepath.Join(s.cfg.RuntimeDir, "device-auth"))
		if err != nil {
			return fmt.Errorf("open device authentication read-only after Tailscale readiness: %w", err)
		}
		if err := s.attachDeviceStore(store); err != nil {
			return err
		}
		if err := s.bootstrapGate.Attach(store); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) checkManagedTailscaleReadiness(ctx context.Context, verifyBundle bool) error {
	if !s.managedLocalReady() {
		return errors.New("local relay readiness is incomplete")
	}
	if err := s.validateTailscaleOwner(ctx); err != nil {
		return err
	}
	if err := s.checkManagedTailscaleReadinessWithoutOwner(ctx, verifyBundle); err != nil {
		return err
	}
	return s.validateTailscaleOwner(ctx)
}

// checkManagedTailscaleReadinessWithoutOwner is used only while
// SessionAuthority.WithValidatedRoute holds its operation lock across the
// health pass and invitation commit. That method performs live route
// validation both before and after this callback.
func (s *Server) checkManagedTailscaleReadinessWithoutOwner(ctx context.Context, verifyBundle bool) error {
	if !s.managedLocalReady() {
		return errors.New("local relay readiness is incomplete")
	}
	if s.udp == nil {
		return errors.New("managed UDP listener is unavailable")
	}
	if s.webH == nil || s.webH.BundleVersion() != s.version || s.webH.BundleRevision() != s.revision {
		return errors.New("local web bundle identity does not match the relay binary")
	}
	if err := s.checkLocalHealth(ctx); err != nil {
		return err
	}
	origin, ok := s.tailscaleSession.Origin()
	if !ok || origin != s.cfg.TailscaleOrigin {
		return errors.New("prepared Tailscale origin changed")
	}
	if err := s.checkPublicHealth(ctx, origin); err != nil {
		return err
	}
	if verifyBundle {
		if err := appdeploy.VerifyPublic(ctx, s.cfg.WebRoot, origin, s.version, s.revision); err != nil {
			return fmt.Errorf("public HTTPS bundle verification failed: %w", err)
		}
	}
	return nil
}

func (s *Server) checkLocalHealth(ctx context.Context) error {
	checkCtx, cancel := context.WithTimeout(ctx, managedHealthTimeout)
	defer cancel()
	client := managedHealthClientForServer(s, managedHealthTimeout)
	request, err := http.NewRequestWithContext(checkCtx, http.MethodGet, "http://"+s.cfg.Addr()+"/readyz", nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("local relay health check failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("local relay is not ready (HTTP %d)", response.StatusCode)
	}
	var health struct {
		Status string `json:"status"`
	}
	if err := decodeManagedHealth(response.Body, &health); err != nil || health.Status != "ready" {
		return errors.New("local relay readiness response was invalid")
	}
	return nil
}

func (s *Server) checkPublicHealth(ctx context.Context, origin string) error {
	checkCtx, cancel := context.WithTimeout(ctx, managedHealthTimeout)
	defer cancel()
	client := managedHealthClientForServer(s, managedHealthTimeout)
	request, err := http.NewRequestWithContext(checkCtx, http.MethodGet, strings.TrimSuffix(origin, "/")+"/healthz", nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("trusted public HTTPS health check failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("X-Herdr-Relay-Instance") != s.cfg.InstanceID {
		return errors.New("public HTTPS health endpoint identity did not match this relay")
	}
	var health struct {
		Status    string `json:"status"`
		Readiness string `json:"readiness"`
		Transport string `json:"transport"`
		Instance  string `json:"instance"`
		Version   string `json:"version"`
		Revision  string `json:"revision"`
		Origin    string `json:"tailscale_origin"`
	}
	if err := decodeManagedHealth(response.Body, &health); err != nil {
		return errors.New("public HTTPS health response was invalid")
	}
	if health.Status != "ok" || health.Readiness != "ready" || health.Transport != config.TransportTailscale ||
		health.Instance != s.cfg.InstanceID || health.Version != s.version || health.Revision != s.revision || health.Origin != origin {
		return errors.New("public HTTPS health identity or readiness did not match this relay")
	}
	return nil
}

func decodeManagedHealth(body io.Reader, destination any) error {
	const maxHealthBytes = 64 << 10
	data, err := io.ReadAll(io.LimitReader(body, maxHealthBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxHealthBytes {
		return errors.New("managed health response is too large")
	}
	return json.Unmarshal(data, destination)
}

func managedHealthClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		Transport:     &http.Transport{Proxy: nil, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func (s *Server) isTailscaleQuarantined() bool {
	if s == nil || s.tailscaleSession == nil {
		return false
	}
	s.mu.RLock()
	quarantined := s.quarantined
	s.mu.RUnlock()
	return quarantined
}

func (s *Server) quarantineTailscale() {
	if s == nil || s.tailscaleSession == nil {
		return
	}
	s.quarantineTailscaleLocked()
}

func (s *Server) quarantineTailscaleLocked() {
	if s.bootstrapGate != nil {
		s.bootstrapGate.Revoke()
	}
	s.mu.Lock()
	s.quarantined = true
	s.mu.Unlock()
	if s.hub != nil {
		s.hub.RevokeAdmission()
	}
}

func (s *Server) quarantineTailscaleContext(ctx context.Context) error {
	if s == nil || s.tailscaleSession == nil || ctx == nil {
		return errors.New("managed Tailscale session is unavailable")
	}
	if s.bootstrapGate != nil {
		s.bootstrapGate.Revoke()
	}
	s.mu.Lock()
	s.quarantined = true
	s.mu.Unlock()
	if s.hub == nil {
		return nil
	}
	// Fence admission and disconnect already-authenticated sessions before
	// waiting on the context-bound lifecycle lock. A timeout must not return
	// with an existing Hub client still admitted to the managed backend.
	s.hub.RevokeAdmission()
	return nil
}

// RetireManagedTailscale revokes pairing first, then asks the real in-process
// owner to reconcile only its exact route. O remains held on every unresolved
// outcome; successful route clearing and joined local watch closure are the
// only conditions that allow Run to finish and release O.
func (s *Server) RetireManagedTailscale(ctx context.Context) error {
	if s == nil || s.tailscaleSession == nil || ctx == nil {
		return errors.New("managed Tailscale session is unavailable")
	}
	// Revoke authentication and inert HTTP first, then establish the Hub
	// admission fence before waiting on the context-bound lifecycle lock.
	if err := s.quarantineTailscaleContext(ctx); err != nil {
		return fmt.Errorf("close managed Tailscale admission before retirement: %w", err)
	}
	unlock, err := s.tailscaleOpMu.Lock(ctx)
	if err != nil {
		return fmt.Errorf("wait for managed Tailscale operation before retirement: %w", err)
	}
	defer unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	status := s.tailscaleSession.Status()
	if status.RouteCleared && status.LocalWatchClosed {
		return nil
	}
	if s.managedOwner == nil {
		return errors.New("managed owner is unavailable for Tailscale retirement")
	}
	if err := s.managedOwner.Validate(); err != nil {
		return fmt.Errorf("managed owner validation failed before Tailscale retirement: %w", err)
	}
	err = s.tailscaleSession.Retire(ctx)
	status = s.tailscaleSession.Status()
	if status.RouteCleared && status.LocalWatchClosed {
		return nil
	}
	if err != nil {
		return fmt.Errorf("Tailscale route retirement remains unresolved: %w", err)
	}
	return errors.New("Tailscale route retirement remains unresolved")
}

// CompleteManagedTailscaleRetirement releases Run's wait only after a safe
// retirement acknowledgement has been written to the requesting private
// control connection (or a signal-driven caller observes success).
func (s *Server) CompleteManagedTailscaleRetirement() {
	if s == nil || s.tailscaleSession == nil || !s.ManagedOwnerReleaseSafe() {
		return
	}
	s.managedRetireOne.Do(func() { close(s.managedRetired) })
}

// ManagedOwnerReleaseSafe is checked by runServe immediately before O release.
func (s *Server) ManagedOwnerReleaseSafe() bool {
	if s == nil || s.tailscaleSession == nil {
		return true
	}
	status := s.tailscaleSession.Status()
	return status.RouteCleared && status.LocalWatchClosed
}

func (s *Server) controlStatus(ctx context.Context) localcontrol.Status {
	if s.cfg.Transport == config.TransportTailscaleExternal {
		return s.externalControlStatus()
	}
	status := localcontrol.Status{
		Transport: s.cfg.Transport,
		Version:   s.version,
		Revision:  s.revision,
	}
	if s.webH != nil {
		status.BundleHash = s.webH.BundleHash()
	}
	s.mu.RLock()
	backendBound := s.backendBound
	quarantined := s.quarantined
	s.mu.RUnlock()
	status.LocalReady = backendBound && s.udp != nil && s.state != nil && s.state.InventoryReady()
	status.OwnerHeld = s.managedOwner != nil && s.managedOwner.Validate() == nil
	if s.tailscaleSession == nil {
		status.Ready = s.ready
		status.LocalReady = s.ready
		status.ServeReady = s.ready
		store := s.deviceStore()
		if store != nil {
			invitation := store.BootstrapStatus()
			status.InvitationArmed = invitation.Armed
			status.InvitationPending = invitation.Pending
			if !invitation.ExpiresAt.IsZero() {
				status.InvitationExpiresAt = invitation.ExpiresAt.UTC().Format(time.RFC3339)
			}
		}
		return status
	}
	ownerState := s.tailscaleSession.Status()
	status.Quarantined = quarantined || ownerState.Quarantined || ownerState.Invalidated
	if !status.OwnerHeld {
		s.quarantineTailscale()
		status.Quarantined = true
	}
	status.RouteCleared = ownerState.RouteCleared
	status.LocalWatchClosed = ownerState.LocalWatchClosed
	status.RemoteWatchRetirementUnknown = ownerState.RemoteWatchRetirementUnknown
	status.RegistrationOutcome = ownerState.RegistrationOutcome
	if ownerState.Active && !status.Quarantined && status.OwnerHeld && status.LocalReady {
		validateCtx, cancel := context.WithTimeout(ctx, localcontrol.StatusTimeout)
		err := s.checkManagedTailscaleReadiness(validateCtx, false)
		cancel()
		if err == nil {
			status.ServeReady = true
		} else {
			s.quarantineTailscale()
			status.Quarantined = true
		}
	}
	store := s.deviceStore()
	if store != nil {
		invitation := store.BootstrapStatus()
		status.InvitationArmed = invitation.Armed
		status.InvitationPending = invitation.Pending
		if !invitation.ExpiresAt.IsZero() {
			status.InvitationExpiresAt = invitation.ExpiresAt.UTC().Format(time.RFC3339)
		}
	}
	status.Ready = status.ServeReady && s.bootstrapGate != nil && s.bootstrapGate.OpenStatus() && !status.Quarantined
	return status
}

func (s *Server) activateForControl(ctx context.Context) (localcontrol.Status, error) {
	return s.activateTailscale(ctx)
}

func (s *Server) armForControl(ctx context.Context) (localcontrol.Status, error) {
	if s.tailscaleSession != nil {
		return s.armManagedTailscale(ctx)
	}
	if s.cfg.Transport == config.TransportTailscaleExternal {
		return s.armExternalTailscale(ctx)
	}
	return s.armBootstrapForControl()
}

func (s *Server) controlRunID() string {
	if s.cfg.Transport == config.TransportTailscaleExternal {
		return s.cfg.ControlRunID
	}
	return s.cfg.ManagedRunID
}

func (s *Server) externalControlStatus() localcontrol.Status {
	status := localcontrol.Status{
		Transport:      s.cfg.Transport,
		Version:        s.version,
		Revision:       s.revision,
		PhoneAppOrigin: s.cfg.PhoneAppOrigin,
	}
	if s.webH != nil {
		status.BundleHash = s.webH.BundleHash()
	}
	s.mu.RLock()
	backendBound := s.backendBound
	s.mu.RUnlock()
	status.LocalReady = backendBound && s.udp != nil && s.state != nil && s.state.InventoryReady()
	status.Ready = status.LocalReady && s.bootstrapGate != nil && s.bootstrapGate.OpenStatus()
	if store := s.deviceStore(); store != nil {
		invitation := store.BootstrapStatus()
		status.InvitationArmed = invitation.Armed
		status.InvitationPending = invitation.Pending
		if !invitation.ExpiresAt.IsZero() {
			status.InvitationExpiresAt = invitation.ExpiresAt.UTC().Format(time.RFC3339)
		}
	}
	return status
}

func (s *Server) retireForControl(ctx context.Context) (localcontrol.Status, error) {
	if err := s.RetireManagedTailscale(ctx); err != nil {
		return s.controlStatus(context.Background()), err
	}
	return s.controlStatus(ctx), nil
}
