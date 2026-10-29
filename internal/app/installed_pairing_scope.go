package app

import (
	"context"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/deviceauth"
	"github.com/0cv/herdr-mobile-relay/internal/localcontrol"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

// Private shared physical scope, not admission or an already-owned API.
// The optional observer sees evaluation of the lock's cancellation channel
// while the real backend lease is held. It cannot replace admission, the
// context, lock acquisition, or the operation.
func (s *Server) withInstalledPairingScope(call context.Context, operation func(context.Context) error, waiting ...func()) error {
	b := s.installedBackend
	if b == nil {
		return tailscalecli.ErrWorkflowRequired
	}
	return b.WithLease(call, func() error {
		joined, cancel := context.WithCancel(call)
		cancelled := make(chan struct{})
		stop := context.AfterFunc(b.lifetime, func() {
			defer close(cancelled)
			cancel()
		})
		defer func() {
			if !stop() {
				<-cancelled
			}
		}()
		defer cancel()
		if b.lifetime.Err() != nil {
			cancel()
		}
		queueContext := context.Context(joined)
		if len(waiting) != 0 && waiting[0] != nil {
			queueContext = installedPairingQueueObservation{Context: joined, waiting: waiting[0]}
		}
		unlock, err := s.externalArmMu.Lock(queueContext)
		if err != nil {
			return err
		}
		defer unlock()
		if err := joined.Err(); err != nil {
			return err
		}
		return operation(joined)
	})
}

type installedPairingQueueObservation struct {
	context.Context
	waiting func()
}

func (c installedPairingQueueObservation) Done() <-chan struct{} {
	c.waiting()
	return c.Context.Done()
}

// Separate from the concrete owner boundary: lower-level synthetic tests can
// exercise late guard transitions without inventing a qualified owner.
func (s *Server) installedPairingRelease(store *deviceauth.Store, result installedPairingPayload) (installedPairingPayload, error) {
	if s.installedAdmissionRevoked() || s.bootstrapGate == nil || !s.bootstrapGate.OpenStatus() || store == nil || s.deviceStore() != store {
		return installedPairingPayload{}, deviceauth.ErrBootstrapGateClosed
	}
	return result, nil
}

// Only called after original selection and concrete ownership validation.
// Cancellation is classified per branch, never used to hide terminal siblings.
func (s *Server) recordInstalledPairingFailure(err error) {
	if err == nil || installedFailureBranch(err, false, true) {
		return
	}
	s.recordInstalledRecoveryFailure(context.Background(), err)
}

func (s *Server) installedPairingAdmission(status localcontrol.Status, err error) error {
	if err == nil && !status.Ready {
		if !status.LocalReady || status.PersistentRouteReadiness == string(tailscalecli.ReadinessWaiting) {
			err = tailscalecli.ErrTransientUnavailable
		} else {
			err = tailscalecli.ErrUnsupported
		}
	}
	if err != nil {
		s.recordInstalledPairingFailure(err)
		if installedAvailabilityFailure(err) {
			s.quarantineTailscaleCLI()
		}
	}
	return err
}

// Point-in-time lifetime observation, NOT atomic network delivery.
func (s *Server) installedPairingResultBoundary(ctx context.Context, selected config.InstalledConfig) (err error) {
	// Binding validation may perform filesystem reads; observe lifetimes again
	// after those reads, directly rather than through AfterFunc propagation.
	defer func() {
		if err != nil {
			return
		}
		if ctx.Err() != nil {
			err = ctx.Err()
			return
		}
		b := s.installedBackend
		if b == nil {
			err = tailscalecli.ErrWorkflowRequired
			return
		}
		b.mu.Lock()
		live := b.active && b.lifetime.Err() == nil
		b.mu.Unlock()
		if a := s.installedSetup; a != nil {
			a.mu.Lock()
			live = live && a.active && a.lifetime.Err() == nil
			a.mu.Unlock()
		}
		if !live {
			err = tailscalecli.ErrWorkflowRequired
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	b := s.installedBackend
	if b == nil {
		return tailscalecli.ErrWorkflowRequired
	}
	b.mu.Lock()
	active := b.active && b.lifetime.Err() == nil
	b.mu.Unlock()
	if !active {
		return tailscalecli.ErrWorkflowRequired
	}
	if a := s.installedSetup; a != nil {
		a.mu.Lock()
		active := a.active && a.lifetime.Err() == nil && a.backend == b && a.binding == selected.Binding()
		a.mu.Unlock()
		if !active || !a.registered() || s.tailscaleCLIRegistration != a {
			return tailscalecli.ErrWorkflowRequired
		}
		return a.session.ValidateRuntimeBindingContext(ctx, selected.Binding())
	}
	if s.installedCLI == nil || s.tailscaleCLIRegistration != s.installedCLI {
		return tailscalecli.ErrWorkflowRequired
	}
	return s.installedCLI.ValidateRuntimeBinding(selected.Binding())
}
