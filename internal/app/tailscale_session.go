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
	inspection, err := tailscale.Inspect(ctx, "tailscale", port)
	if err != nil {
		return nil, fmt.Errorf("read-only Tailscale inspection failed: %w", err)
	}
	if !inspection.LoggedIn || !inspection.ServeInspected || !inspection.ExposureComplete ||
		inspection.ServeConfigured || inspection.FunnelConfigured || inspection.ServeRouteCount != 0 ||
		len(inspection.ServeRoutes) != 0 || inspection.ServeRouteOwned {
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
	if s == nil || s.tailscaleSession == nil || s.managedOwner == nil {
		return errors.New("managed Tailscale owner is unavailable")
	}
	if err := s.managedOwner.Validate(); err != nil {
		return fmt.Errorf("managed owner validation failed: %w", err)
	}
	if err := s.tailscaleSession.Validate(ctx); err != nil {
		return fmt.Errorf("live Tailscale route validation failed: %w", err)
	}
	return nil
}

func (s *Server) activateTailscale(ctx context.Context) (localcontrol.Status, error) {
	s.tailscaleOpMu.Lock()
	defer s.tailscaleOpMu.Unlock()
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
	if err := s.checkManagedTailscaleReadiness(ctx, true); err != nil {
		s.quarantineTailscale()
		return s.pairingControlStatusContext(ctx), err
	}
	return s.pairingControlStatusContext(ctx), nil
}

func (s *Server) armManagedTailscale(ctx context.Context) (localcontrol.Status, error) {
	s.tailscaleOpMu.Lock()
	defer s.tailscaleOpMu.Unlock()
	if err := s.checkManagedTailscaleReadiness(ctx, true); err != nil {
		s.quarantineTailscale()
		return s.pairingControlStatusContext(ctx), err
	}
	if err := s.ensureManagedDeviceStore(); err != nil {
		return s.pairingControlStatusContext(ctx), err
	}
	if err := s.checkManagedTailscaleReadiness(ctx, false); err != nil {
		s.quarantineTailscale()
		return s.pairingControlStatusContext(ctx), err
	}
	select {
	case <-s.tailscaleSession.Invalidation():
		s.quarantineTailscale()
		return s.pairingControlStatusContext(ctx), errors.New("Tailscale owner was invalidated before invitation persistence")
	default:
	}
	store := s.deviceStore()
	if err := store.ArmBootstrapInvitation([]byte(s.cfg.Token), s.hostname, "en"); err != nil {
		s.recordSafeError("bootstrap invitation arm failed", err)
		return s.pairingControlStatusContext(ctx), err
	}
	if err := s.checkManagedTailscaleReadiness(ctx, false); err != nil {
		s.quarantineTailscale()
		return s.pairingControlStatusContext(ctx), err
	}
	select {
	case <-s.tailscaleSession.Invalidation():
		s.quarantineTailscale()
		return s.pairingControlStatusContext(ctx), errors.New("Tailscale owner was invalidated before pairing arm")
	default:
	}
	if err := s.bootstrapGate.Open(); err != nil {
		return s.pairingControlStatusContext(ctx), err
	}
	s.hub.SetAccepting(true)
	s.mu.Lock()
	s.quarantined = false
	s.mu.Unlock()
	status := s.pairingControlStatusContext(ctx)
	if !status.Ready || !status.InvitationArmed {
		s.quarantineTailscale()
		return s.pairingControlStatusContext(ctx), errors.New("pairing readiness was not acknowledged")
	}
	return status, nil
}

func (s *Server) ensureManagedDeviceStore() error {
	store := s.deviceStore()
	if store == nil {
		var err error
		store, err = deviceauth.Open(filepath.Join(s.cfg.RuntimeDir, "device-auth"))
		if err != nil {
			return fmt.Errorf("initialize device authentication after Tailscale readiness: %w", err)
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
	return s.validateTailscaleOwner(ctx)
}

func (s *Server) checkLocalHealth(ctx context.Context) error {
	checkCtx, cancel := context.WithTimeout(ctx, managedHealthTimeout)
	defer cancel()
	client := managedHealthClient(managedHealthTimeout)
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
	client := managedHealthClient(managedHealthTimeout)
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
	if s.bootstrapGate != nil {
		s.bootstrapGate.Revoke()
	}
	if s.hub != nil {
		s.hub.SetAccepting(false)
	}
	s.mu.Lock()
	s.quarantined = true
	s.mu.Unlock()
}

// RetireManagedTailscale revokes pairing first, then asks the real in-process
// owner to reconcile only its exact route. O remains held on every unresolved
// outcome; successful route clearing and joined local watch closure are the
// only conditions that allow Run to finish and release O.
func (s *Server) RetireManagedTailscale(ctx context.Context) error {
	if s == nil || s.tailscaleSession == nil || ctx == nil {
		return errors.New("managed Tailscale session is unavailable")
	}
	s.tailscaleOpMu.Lock()
	defer s.tailscaleOpMu.Unlock()
	s.quarantineTailscale()
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
	err := s.tailscaleSession.Retire(ctx)
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
	return s.armBootstrapForControl()
}

func (s *Server) retireForControl(ctx context.Context) (localcontrol.Status, error) {
	if err := s.RetireManagedTailscale(ctx); err != nil {
		return s.controlStatus(context.Background()), err
	}
	return s.controlStatus(ctx), nil
}
