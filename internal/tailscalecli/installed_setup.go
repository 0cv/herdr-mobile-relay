package tailscalecli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// InstalledSetupProposal is configuration/attempt data, NOT operator authentication.
// S2 must authenticate the operator independently of public health identity.
type InstalledSetupProposal struct {
	Binding                             InstalledBinding
	NodeID, ReservationID, Confirmation string
	Profile                             Profile
}

func (InstalledSetupProposal) String() string     { return "installed setup proposal (redacted)" }
func (p InstalledSetupProposal) GoString() string { return p.String() }

// InstalledSetupSession is callback-scoped ownership, not an exported policy bypass.
// The caller must close/join its backend before returning from the callback.
type InstalledSetupSession struct {
	mu           sync.Mutex
	operations   sync.WaitGroup
	serial       chan struct{}
	active       bool
	lifetime     context.Context
	binding      InstalledBinding
	manager      *Manager
	inspection   Inspection
	reservation  backendPortReservation
	receipt      *registration
	acknowledged *backendPortReservation
	// Only observation may be replaced by package tests; consent, ownership and lease cannot.
	runtimeGeneration  string
	observeReadiness   func(context.Context) error
	observeQueue       func()
	beforeDurableWrite func(string) error
	// Private acknowledgement fault/observation seam, after real coordination release.
	afterAcknowledgementCoordination func() error
}

func (m *Manager) WithInstalledSetupSession(ctx context.Context, binding InstalledBinding, run func(context.Context, *InstalledSetupSession) error) error {
	if m == nil || m.client == nil || ctx == nil || run == nil {
		return ErrWorkflowRequired
	}
	in := binding.input
	// Gate before CLI or filesystem effects, including for invalid bindings.
	if err := m.requireDevelopmentTuple("production", in.HTTPSPort, in.BackendPort); err != nil {
		return err
	}
	if err := binding.Validate(); err != nil {
		return err
	}
	if binding.roots[0] != m.stateRoot || binding.roots[1] != m.coordinationRoot || in.Binary != m.client.binary {
		return ErrWorkflowRequired
	}
	return m.withLifetimeOwnership(ctx, in.BackendPort, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		initial, err := m.client.Inspect(ctx)
		if err != nil {
			return err
		}
		if err := m.requireProfileForScope(initial, "production"); err != nil {
			return err
		}
		attempt, err := newOperationID()
		if err != nil {
			return err
		}
		lifetime, cancel := context.WithCancel(ctx)
		s := &InstalledSetupSession{serial: make(chan struct{}, 1), active: true, lifetime: lifetime, binding: binding, manager: m, inspection: initial,
			reservation: backendPortReservation{Schema: 1, InstallationID: in.InstallationID, Scope: "production", NodeID: initial.Identity.NodeID, HTTPSPort: in.HTTPSPort, BackendPort: in.BackendPort, Origin: in.Origin, ReservationID: attempt, State: StatePublishPending, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano)}}
		defer func() { s.mu.Lock(); s.active = false; cancel(); s.mu.Unlock(); s.operations.Wait() }()
		err = s.coordinate(ctx, func() error {
			if err := binding.Validate(); err != nil {
				return err
			}
			record, err := m.readRegistration()
			if err != nil {
				return err
			}
			if record != nil {
				return ErrUncertain
			}
			reserved, err := m.readBackendReservation(in.BackendPort)
			if err != nil {
				return err
			}
			if reserved != nil {
				return ErrConflict
			}
			fresh, err := m.client.Inspect(ctx)
			if err != nil {
				return err
			}
			if !sameInspectionIdentity(initial, fresh) || !originMatchesIdentity(in.Origin, fresh.Identity, in.HTTPSPort) || routeConflicts(fresh.Serve, in.HTTPSPort) || backendPortHasRoute(fresh.Serve, in.BackendPort, nil) {
				return ErrConflict
			}
			if err := m.requireProfileForScope(fresh, "production"); err != nil {
				return err
			}
			if err := binding.Validate(); err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			// Pin absence across the observation, not just before it.
			record, err = m.readRegistration()
			if err != nil {
				return err
			}
			if record != nil {
				return ErrUncertain
			}
			reserved, err = m.readBackendReservation(in.BackendPort)
			if err != nil {
				return err
			}
			if reserved != nil {
				return ErrConflict
			}
			return m.writeBackendReservation(s.reservation)
		})
		if err != nil {
			return err
		}
		if err := lifetime.Err(); err != nil {
			return err
		}
		return run(lifetime, s)
	})
}

func (s *InstalledSetupSession) coordinate(ctx context.Context, fn func() error) error {
	return s.manager.withNodeLock(ctx, s.inspection.Identity.NodeID, func() error { return s.manager.withBackendReservationLock(ctx, fn) })
}
func (s *InstalledSetupSession) operation(ctx context.Context, binding InstalledBinding, fn func(context.Context) error) error {
	if s == nil {
		return ErrWorkflowRequired
	}
	s.mu.Lock()
	if !s.active || s.manager == nil || binding != s.binding {
		s.mu.Unlock()
		return ErrWorkflowRequired
	}
	s.operations.Add(1)
	s.mu.Unlock()
	defer s.operations.Done()
	if ctx == nil {
		ctx = context.Background()
	}
	call, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	stop := context.AfterFunc(s.lifetime, func() { defer close(done); cancel() })
	defer func() {
		if !stop() {
			<-done
		}
	}()
	defer cancel()
	// Admission remains counted while queued; retirement drains active work too.
	if s.observeQueue != nil {
		s.observeQueue() // observation only; installed by tagged fixtures
	}
	select {
	case s.serial <- struct{}{}:
		defer func() { <-s.serial }()
	case <-call.Done():
		return call.Err()
	case <-s.lifetime.Done():
		return s.lifetime.Err()
	}
	if s.lifetime.Err() != nil {
		cancel()
	}
	if err := call.Err(); err != nil {
		return err
	}
	if err := binding.Validate(); err != nil {
		return err
	}
	err := fn(call)
	if s.lifetime.Err() != nil {
		cancel()
	}
	return errors.Join(err, call.Err())
}
func (s *InstalledSetupSession) Proposal(ctx context.Context, binding InstalledBinding) (InstalledSetupProposal, error) {
	var p InstalledSetupProposal
	err := s.operation(ctx, binding, func(context.Context) error {
		p = InstalledSetupProposal{Binding: s.binding, NodeID: s.inspection.Identity.NodeID, Profile: s.inspection.Profile, ReservationID: s.reservation.ReservationID}
		p.Confirmation = fmt.Sprintf("PUBLISH INSTALLED ROUTE scope=production node=%s origin=%s https-port=%d backend=127.0.0.1:%d attempt=%s", p.NodeID, binding.input.Origin, binding.input.HTTPSPort, binding.input.BackendPort, p.ReservationID)
		return nil
	})
	return p, err
}
func (s *InstalledSetupSession) pending() error {
	if err := s.binding.Validate(); err != nil {
		return err
	}
	r, err := s.manager.readRegistration()
	if err != nil {
		return err
	}
	if r != nil {
		return ErrUncertain
	}
	b, err := s.manager.readBackendReservation(s.reservation.BackendPort)
	if err != nil {
		return err
	}
	if b == nil || *b != s.reservation {
		return ErrConflict
	}
	return nil
}

// BindRuntimeGeneration pins a PUBLIC correlation identity for this owner.
// It conveys no capability, readiness or qualification.
func (s *InstalledSetupSession) BindRuntimeGeneration(ctx context.Context, binding InstalledBinding, generation string) error {
	return s.operation(ctx, binding, func(context.Context) error {
		if generation == "" || !validLabel(generation) || (s.runtimeGeneration != "" && s.runtimeGeneration != generation) {
			return ErrWorkflowRequired
		}
		s.runtimeGeneration = generation
		return nil
	})
}

// CheckReadiness observes the original pending reservation and local backend,
// under the owned session and physical lease. It never consumes the attempt.
// The existing tagged readiness fixture is also used by this preflight.
func (s *InstalledSetupSession) CheckReadiness(ctx context.Context, binding InstalledBinding, lease BackendLease) error {
	if lease == nil {
		return ErrWorkflowRequired
	}
	return lease.WithLease(ctx, func() error {
		return s.operation(ctx, binding, func(ctx context.Context) error {
			if err := s.pending(); err != nil {
				return err
			}
			if err := s.readiness(ctx); err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			return s.pending()
		})
	})
}

func (s *InstalledSetupSession) readiness(ctx context.Context) error {
	if s.observeReadiness != nil && s.manager.fixtureMutations {
		return s.observeReadiness(ctx)
	}
	in := s.binding.input
	return verifyBackendReadiness(ctx, in.BackendPort, in.InstallationID, in.Origin, s.runtimeGeneration)
}

// Publish requires in-process service lease authority and exact installed consent.
// It is not an authenticated external control endpoint and never retries a receipt.
func (s *InstalledSetupSession) Publish(ctx context.Context, binding InstalledBinding, consent Consent, lease BackendLease) error {
	p, err := s.Proposal(ctx, binding)
	if err != nil {
		return err
	}
	return s.operation(ctx, binding, func(ctx context.Context) error {
		in := binding.input
		if err := s.manager.requireDevelopmentTuple("production", in.HTTPSPort, in.BackendPort); err != nil {
			return err
		}
		if lease == nil || consent.ReservationID != p.ReservationID || consent.RouteConfirmation != p.Confirmation {
			return ErrWorkflowRequired
		}
		if err := validatePublishConsent(consent, p.NodeID, "production", in.Origin, in.HTTPSPort, in.BackendPort); err != nil {
			return err
		}
		request := PublishRequest{InstallationID: in.InstallationID, Scope: "production", ExpectedNodeID: p.NodeID, Origin: in.Origin, HTTPSPort: in.HTTPSPort, BackendPort: in.BackendPort, ReservationID: p.ReservationID, Consent: consent, BackendLease: lease}
		return lease.WithLease(ctx, func() error {
			return s.coordinate(ctx, func() error {
				if err := s.manager.publishTransaction(ctx, request, s.inspection, s); err != nil {
					return err
				}
				record, err := s.manager.readRegistration()
				if err != nil || record == nil {
					return ErrUncertain
				}
				reservation, err := s.manager.readInstalledReservation(record)
				if err != nil {
					return err
				}
				s.receipt = record
				s.acknowledged = reservation
				return binding.Validate()
			})
		})
	})
}

// VerifyRegisteredRoute never acquires lifetime ownership or changes evidence.
func (s *InstalledSetupSession) VerifyRegisteredRoute(ctx context.Context, binding InstalledBinding) (RouteStatus, error) {
	status := RouteStatus{Readiness: ReadinessUncertain}
	err := s.operation(ctx, binding, func(ctx context.Context) error {
		return s.manager.withOwnedReadOnlyCoordination(ctx, s.inspection.Identity.NodeID, func() error {
			validate := func() error {
				if err := binding.Validate(); err != nil {
					return err
				}
				r, err := s.manager.readRegistration()
				if err != nil || r == nil || s.receipt == nil || *r != *s.receipt {
					return ErrUncertain
				}
				b, err := s.manager.readInstalledReservation(r)
				if err != nil || s.acknowledged == nil || *b != *s.acknowledged {
					return ErrUncertain
				}
				return nil
			}
			if err := validate(); err != nil {
				return err
			}
			in := binding.input
			var err error
			status, err = s.manager.VerifyRegisteredRoute(ctx, "production", in.InstallationID, in.Origin, in.HTTPSPort, in.BackendPort)
			if after := validate(); after != nil {
				status.Readiness = ReadinessUncertain
				return errors.Join(err, after, ctx.Err())
			}
			return errors.Join(err, ctx.Err())
		})
	})
	return status, err
}

// PrepareRuntime creates only missing private app leaves inside setup ownership.
func (s *InstalledSetupSession) PrepareRuntime(binding InstalledBinding) error {
	return s.operation(nil, binding, func(context.Context) error {
		for _, root := range []string{binding.input.RuntimeRoot, binding.input.CacheRoot} {
			if err := os.MkdirAll(root, 0700); err != nil {
				return err
			}
		}
		return binding.Validate()
	})
}

// ValidateRuntimeBinding exposes ownership sufficient for later closed-app construction,
// not route admission. S1b supplies application preparation and backend retirement.
func (s *InstalledSetupSession) ValidateRuntimeBinding(binding InstalledBinding) error {
	return s.ValidateRuntimeBindingContext(context.Background(), binding)
}

// ValidateRuntimeBindingContext preserves request cancellation while queued
// behind an owned session operation. Legacy construction retains its API.
func (s *InstalledSetupSession) ValidateRuntimeBindingContext(ctx context.Context, binding InstalledBinding) error {
	if ctx == nil {
		return ErrWorkflowRequired
	}
	return s.operation(ctx, binding, func(context.Context) error { return nil })
}
