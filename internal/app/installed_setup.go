package app

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

// InstalledSetupControl is callback-scoped in-process authority, NOT operator
// authentication. It exposes neither the service writer nor a caller-set lease.
// Copies share retirement; the zero value and retained handles refuse.
type InstalledSetupControl struct {
	state         *installedSetupState
	beforePublish func(context.Context) error
}

type installedSetupState struct {
	mu         sync.Mutex
	publishMu  contextLock
	active     bool
	attempted  bool
	failed     bool
	published  chan struct{}
	operations sync.WaitGroup
	lifetime   context.Context
	cancel     context.CancelFunc
	session    *tailscalecli.InstalledSetupSession
	binding    tailscalecli.InstalledBinding
	backend    *installedBackend
	cfg        config.Config
}

func (h InstalledSetupControl) operation(ctx context.Context, fn func(context.Context, *installedSetupState) error) error {
	a := h.state
	if a == nil || ctx == nil {
		return tailscalecli.ErrWorkflowRequired
	}
	a.mu.Lock()
	if !a.active {
		a.mu.Unlock()
		return tailscalecli.ErrWorkflowRequired
	}
	a.operations.Add(1)
	a.mu.Unlock()
	defer a.operations.Done()
	call, cancel := context.WithCancel(ctx)
	stop := installedCancelBridge(a.lifetime, cancel)
	defer stop()
	defer cancel()
	if a.lifetime.Err() != nil {
		cancel()
	}
	if err := call.Err(); err != nil {
		return err
	}
	return fn(call, a)
}

func (h InstalledSetupControl) Proposal(ctx context.Context) (p tailscalecli.InstalledSetupProposal, err error) {
	err = h.operation(ctx, func(ctx context.Context, a *installedSetupState) error {
		var err error
		p, err = a.session.Proposal(ctx, a.binding)
		return err
	})
	return
}

// Publish is explicit and one-shot after owned local readiness. Unavailable or
// cancelled preflight preserves the attempt. Once dispatched, failures require
// a separate lifecycle/recovery decision; disk never upgrades this handle.
func (h InstalledSetupControl) Publish(ctx context.Context, consent tailscalecli.Consent) error {
	return h.operation(ctx, func(ctx context.Context, a *installedSetupState) error {
		unlock, err := a.publishMu.Lock(ctx)
		if err != nil {
			return err
		}
		defer unlock()
		a.mu.Lock()
		backend, attempted := a.backend, a.attempted
		a.mu.Unlock()
		if attempted || backend == nil {
			return tailscalecli.ErrWorkflowRequired
		}
		return backend.WithLease(ctx, func() error {
			call, cancel := context.WithCancel(ctx)
			stop := installedCancelBridge(backend.lifetime, cancel)
			defer stop()
			defer cancel()
			if backend.lifetime.Err() != nil {
				cancel()
			}
			if err := a.session.CheckReadiness(call, a.binding, backend); err != nil {
				return err
			}
			if err := call.Err(); err != nil {
				return err
			}
			a.mu.Lock()
			if !a.active || a.lifetime.Err() != nil || backend.lifetime.Err() != nil {
				a.mu.Unlock()
				return context.Canceled
			}
			a.attempted = true
			a.mu.Unlock()
			if err := a.session.Publish(call, a.binding, consent, backend); err != nil {
				a.mu.Lock()
				a.failed = true
				a.mu.Unlock()
				return err
			}
			close(a.published)
			return nil
		})
	})
}

func (a *installedSetupState) retire() {
	a.mu.Lock()
	a.active = false
	a.cancel()
	a.mu.Unlock()
	a.operations.Wait()
}

// Status reports only this lifetime's acknowledged state, never inferred disk
// recovery. Publication is not HTTPS readiness, admission or authentication.
func (h InstalledSetupControl) Status(ctx context.Context) (state string, err error) {
	err = h.operation(ctx, func(_ context.Context, a *installedSetupState) error {
		state = a.phase()
		return nil
	})
	return
}

func (a *installedSetupState) phase() string {
	if a.registered() {
		return "published-awaiting-read-only-admission"
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failed {
		return "publication-failed-separate-lifecycle-required"
	}
	if a.attempted {
		return "publication-in-progress"
	}
	return "waiting-explicit-publication"
}

func (a *installedSetupState) registered() bool {
	select {
	case <-a.published:
		return true
	default:
		return false
	}
}

// Pinned adapter: ambient tuple arguments cannot change the selected authority.
func (a *installedSetupState) VerifyRegisteredRoute(ctx context.Context, scope, installation, origin string, https, backend int) (tailscalecli.RouteStatus, error) {
	port, err := tailscaleCLIHTTPSPort(a.cfg.TailscaleCLIOrigin)
	if err != nil || scope != a.cfg.TailscaleCLIScope || installation != a.cfg.InstanceID || origin != a.cfg.TailscaleCLIOrigin || https != port || backend != a.cfg.Port || !a.registered() {
		return tailscalecli.RouteStatus{}, tailscalecli.ErrWorkflowRequired
	}
	return a.session.VerifyRegisteredRoute(ctx, a.binding)
}

// RunInstalledSetupCLI retains the original typed selection and owns the only
// writer through cleanup. Control callback completion (including failure) does
// not stop the service; its error is reported at service retirement. Only the
// service context/runner governs lifetime. All control work is joined on exit.
func RunInstalledSetupCLI(ctx context.Context, selected config.InstalledConfig, version, revision string, logger *slog.Logger, manager *tailscalecli.Manager, control func(context.Context, InstalledSetupControl) error) error {
	if control == nil {
		return tailscalecli.ErrWorkflowRequired
	}
	return runInstalledSetupCLI(ctx, selected, version, revision, logger, manager, func(ctx context.Context, _ *Server, h InstalledSetupControl) error { return control(ctx, h) }, nil, nil)
}

func runInstalledSetupCLI(ctx context.Context, selected config.InstalledConfig, version, revision string, logger *slog.Logger, manager *tailscalecli.Manager, control func(context.Context, *Server, InstalledSetupControl) error, endpoint InstalledOperator, beforePublish func(context.Context) error) error {
	if ctx == nil || manager == nil || (control == nil && endpoint == nil) || selected.Binding() == (tailscalecli.InstalledBinding{}) {
		return tailscalecli.ErrWorkflowRequired
	}
	cfg := selected.Snapshot()
	return manager.WithInstalledSetupSession(ctx, selected.Binding(), func(ctx context.Context, session *tailscalecli.InstalledSetupSession) (result error) {
		if err := selected.Validate(&cfg); err != nil {
			return err
		}
		if err := session.ValidateRuntimeBinding(selected.Binding()); err != nil {
			return err
		}
		if err := session.PrepareRuntime(selected.Binding()); err != nil {
			return err
		}
		lifetime, cancel := context.WithCancel(ctx)
		defer cancel()
		operator, retire := context.WithCancel(lifetime)
		a := &installedSetupState{active: true, lifetime: operator, cancel: retire, session: session, binding: selected.Binding(), cfg: cfg, published: make(chan struct{})}
		s := newServerWithStartup(&cfg, version, revision, logger, nil, nil, installedServiceStartup)
		if s.initErr != nil && s.hub == nil {
			return s.initErr
		}
		s.installedSetup = a
		s.tailscaleCLIRegistration = a
		defer func() {
			cancel()
			a.retire()
			s.quarantineTailscaleCLI()
			_ = s.hub.Shutdown(context.Background())
			if s.conversationB != nil {
				result = errors.Join(result, s.conversationB.Close())
			}
		}()
		if s.initErr != nil {
			return s.initErr
		}
		// Pin the sole generation before the runner can expose any health bytes.
		if endpoint != nil {
			c, credentials, err := s.composeInstalledOperator(operator, selected, InstalledSetupControl{state: a, beforePublish: beforePublish})
			if err != nil {
				return err
			}
			defer c.Retire()
			control = func(ctx context.Context, _ *Server, _ InstalledSetupControl) error {
				return invokeInstalledOperator(ctx, c, credentials, endpoint)
			}
		} else {
			if err := s.initializeInstalledGeneration(); err != nil {
				return err
			}
			if err := session.BindRuntimeGeneration(ctx, selected.Binding(), s.controlRunID()); err != nil {
				return err
			}
		}
		done := make(chan struct{})
		var controlErr error
		join := func() { cancel(); <-done }
		defer join()
		go func() {
			defer close(done)
			defer a.retire()
			select {
			case <-lifetime.Done():
				controlErr = lifetime.Err()
				return
			case <-s.listenerBound:
			}
			if err := lifetime.Err(); err != nil {
				controlErr = err
				return
			}
			a.mu.Lock()
			a.backend = s.installedBackend
			a.mu.Unlock()
			controlErr = control(operator, s, InstalledSetupControl{state: a})
		}()
		err := installedCLIRunner()(s, lifetime)
		join() // includes callback retirement and every admitted operation
		return errors.Join(err, controlErr)
	})
}
