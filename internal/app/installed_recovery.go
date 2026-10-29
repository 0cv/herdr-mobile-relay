package app

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"syscall"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/deviceauth"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

// Unknown failures are not evidence of a transient outage. In particular,
// identity, journal, profile and route conflicts require a new service scope.
func installedAvailabilityFailure(err error) bool {
	return installedAvailabilityBranch(err, false)
}

func installedAvailabilityBranch(err error, networkContext bool) bool {
	return installedFailureBranch(err, networkContext, false)
}

func installedFailureBranch(err error, networkContext, operator bool) bool {
	if err == nil {
		return false
	}
	// Classify every joined branch, not the first matching transient cause.
	if many, ok := err.(interface{ Unwrap() []error }); ok {
		children := many.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !installedFailureBranch(child, networkContext, operator) {
				return false
			}
		}
		return true
	}
	if _, certificate := err.(*tls.CertificateVerificationError); certificate {
		return false // Trust/identity failures are not availability evidence.
	}
	// Network context permits only known connection leaves, never arbitrary
	// children. In particular, every branch of a wrapped join still must pass.
	if _, ok := err.(net.Error); ok {
		networkContext = true
	}
	if one, ok := err.(interface{ Unwrap() error }); ok {
		if child := one.Unwrap(); child != nil {
			return installedFailureBranch(child, networkContext, operator)
		}
		// A nil unwrap is a leaf (notably net.DNSError).
	}
	if operator && (err == context.Canceled || err == deviceauth.ErrBootstrapUnavailable || err == deviceauth.ErrInvitationExpired || err == deviceauth.ErrInvitationBurned || err == deviceauth.ErrRateLimited || err == deviceauth.ErrBootstrapGateClosed || err == deviceauth.ErrBootstrapGateCommittedSuspended) {
		return true
	}
	if err == tailscalecli.ErrTransientUnavailable || err == context.DeadlineExceeded {
		return true
	}
	if networkContext {
		switch err {
		case io.EOF, io.ErrUnexpectedEOF, syscall.ECONNRESET, syscall.ECONNREFUSED, syscall.EPIPE,
			syscall.ENETUNREACH, syscall.EHOSTUNREACH, syscall.ENETDOWN, syscall.EHOSTDOWN, syscall.ECONNABORTED:
			return true
		}
	}
	network, ok := err.(net.Error)
	return ok && (network.Timeout() || network.Temporary())
}

// One lifetime loop owns monitoring and recovery. Checks are read-only and
// bounded; full HTTPS/bundle admission runs at most once per 30 seconds, backing
// off to two minutes while unavailable. A conflict latches closed until restart.
// ticks is an inert scheduling seam, not an authority or production bypass.
func installedRecovery(ctx context.Context, ticks <-chan time.Time, check func(context.Context) error, quarantine func()) {
	defer quarantine()
	delay := 30 * time.Second
	var next time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case now, ok := <-ticks:
			if !ok || ctx.Err() != nil {
				return
			}
			if now.Before(next) {
				continue
			}
			checkCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			err := check(checkCtx)
			cancel()
			if ctx.Err() != nil {
				return
			}
			if err == nil {
				delay = 30 * time.Second
			} else {
				quarantine()
				if !installedAvailabilityFailure(err) {
					return
				}
				delay *= 2
				if delay > 2*time.Minute {
					delay = 2 * time.Minute
				}
			}
			next = now.Add(delay)
		}
	}
}

func installedRecoveryAfterPublication(ctx context.Context, published <-chan struct{}, ticks <-chan time.Time, check func(context.Context) error, quarantine func()) {
	if published != nil {
		select {
		case <-ctx.Done():
			quarantine()
			return
		case <-published:
		}
	}
	installedRecovery(ctx, ticks, check, quarantine)
}

func (s *Server) watchInstalledCLI(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	var published <-chan struct{}
	if s.installedSetup != nil {
		published = s.installedSetup.published
	}
	installedRecoveryAfterPublication(ctx, published, ticker.C, func(checkCtx context.Context) (err error) {
		defer func() { s.recordInstalledRecoveryFailure(ctx, err) }()
		status, err := s.admitTailscaleCLI(checkCtx)
		if err != nil {
			return err
		}
		if !status.Ready {
			// The final route observation may lose availability after the
			// HTTPS pass. Retry only after another complete fresh check.
			if !status.LocalReady || status.PersistentRouteReadiness == string(tailscalecli.ReadinessWaiting) {
				return tailscalecli.ErrTransientUnavailable
			}
			return tailscalecli.ErrUnsupported
		}
		return nil
	}, s.quarantineTailscaleCLI)
}

// A terminal monitor result revokes the gate, rather than merely suspending it.
// ResumeAdmission cannot undo Revoke. The latch also rejects future direct
// readiness/admission calls before they perform any observations or store work.
// Service cancellation is not a terminal observation; availability keeps the
// existing reversible suspension/backoff behavior and error-tree precedence.
func (s *Server) recordInstalledRecoveryFailure(service context.Context, err error) {
	if s == nil || (s.installedCLI == nil && s.installedSetup == nil) || service == nil || service.Err() != nil || err == nil || installedAvailabilityFailure(err) {
		return
	}
	s.mu.Lock()
	s.installedTerminal = true
	s.quarantined = true
	s.mu.Unlock()
	if s.bootstrapGate != nil {
		s.bootstrapGate.Revoke()
	}
	if s.hub != nil {
		s.hub.RevokeAdmission()
	}
}

func (s *Server) installedAdmissionRevoked() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.installedTerminal
}
