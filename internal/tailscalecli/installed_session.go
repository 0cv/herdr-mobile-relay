package tailscalecli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
)

// Lock order is instance (canonical private journal root), backend port, node,
// then reservation inventory. Never acquire ownership while holding either
// shared mutation lock. The backend key is host-wide, like reservations, not
// node-scoped: changing nodes must not allow a second local backend owner.
// Mutation transactions and lifetime sessions use the same ownership locks.
func (m *Manager) withLifetimeOwnership(ctx context.Context, backendPort int, operation func() error) error {
	if backendPort < 1 || backendPort > 65535 {
		return ErrConflict
	}
	digest := sha256.Sum256([]byte(m.stateRoot))
	return m.withCoordinationLock(ctx, "instance-"+hex.EncodeToString(digest[:])+".lock", func() error {
		return m.withCoordinationLock(ctx, fmt.Sprintf("backend-owner-%d.lock", backendPort), operation)
	})
}

func (m *Manager) withMutationOwnership(ctx context.Context, backendPort int, nodeID string, operation func() error) error {
	return m.withLifetimeOwnership(ctx, backendPort, func() error {
		return m.withNodeLock(ctx, nodeID, operation)
	})
}

// withOwnedReadOnlyCoordination is only for checks under retained lifetime
// ownership, never publication or acquisition. Even when the nested reservation
// wait cancels, the acquired node lock still runs its ordinary unlock check.
func (m *Manager) withOwnedReadOnlyCoordination(ctx context.Context, nodeID string, operation func() error) error {
	digest := sha256.Sum256([]byte(nodeID))
	return m.coordinateLock(ctx, "node-"+hex.EncodeToString(digest[:])[:24]+".lock", true, func() error {
		return m.coordinateLock(ctx, "backend-reservations.lock", true, operation)
	})
}

// WithInstalledResume holds instance and backend ownership for the whole
// service session. The callback must own and retire the backend/device writer
// before returning; it must not invoke mutation operations (which take the same
// lock). Setup must coordinate with that writer rather than open another store.
// The read-only check is callback-scoped and must not be retained after return.
// Ownership blocks same-instance/backend mutations until backend retirement;
// distinct routes share the node lock only during checks and mutations.
//
// This is a resume primitive, not an installer or a pairing admission check.
// It requires an acknowledged registration and matching reservation. It never
// publishes, repairs, removes, or releases a persistent route on stop. The
// callback still needs exact local/HTTPS/bundle admission before exposing pairing.
// Production profiles remain closed by the manager's existing scope gate.
func (m *Manager) WithInstalledResume(ctx context.Context, installationID, origin string, httpsPort, backendPort int, run func(context.Context, func() (RouteStatus, error)) error) error {
	if run == nil {
		return ErrWorkflowRequired
	}
	return m.withInstalledResume(ctx, installationID, origin, httpsPort, backendPort, func(ctx context.Context, check func(context.Context) (RouteStatus, error)) error {
		return run(ctx, func() (RouteStatus, error) { return check(ctx) })
	})
}

func (m *Manager) withInstalledResume(ctx context.Context, installationID, origin string, httpsPort, backendPort int, run func(context.Context, func(context.Context) (RouteStatus, error)) error) error {
	if m == nil || run == nil {
		return ErrWorkflowRequired
	}
	if err := m.requireDevelopmentTuple("production", httpsPort, backendPort); err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	record, err := m.readRegistration()
	if err != nil {
		return err
	}
	if record == nil || record.State != StateRegistered || !record.MutationAcknowledged || record.ReservationID != "" {
		return ErrUncertain
	}
	return m.acquireInstalledResume(ctx, ctx, record, installationID, origin, httpsPort, backendPort, nil, run)
}

// readInstalledReservation requires the acknowledged lifecycle pairing, not
// merely the tuple comparator used by recovery across lifecycle states.
func (m *Manager) readInstalledReservation(record *registration) (*backendPortReservation, error) {
	reservation, err := m.readBackendReservation(record.BackendPort)
	if err != nil || reservation == nil || reservation.State != StateRegistered || reservation.ReservationID != "" ||
		!sameBackendReservation(*reservation, reservationForRegistration(*record, StateRegistered)) {
		return nil, ErrUncertain
	}
	return reservation, nil
}

// Acquisition deadlines do not govern the owned session lifetime.
func (m *Manager) acquireInstalledResume(acquire, ctx context.Context, record *registration, installationID, origin string, httpsPort, backendPort int, validate func() error, run func(context.Context, func(context.Context) (RouteStatus, error)) error) error {
	return m.withLifetimeOwnership(acquire, backendPort, func() error {
		if err := acquire.Err(); err != nil {
			return err
		}
		// Pin the complete journal across lock acquisition. A replacement must
		// not change which node's authority protects this session.
		current, err := m.readRegistration()
		if err != nil {
			return err
		}
		if current == nil || *current != *record {
			return ErrUncertain
		}
		checkUnlocked := func(ctx context.Context) (RouteStatus, error) {
			if err := ctx.Err(); err != nil {
				return RouteStatus{Readiness: ReadinessWaiting}, err
			}
			current, err := m.readRegistration()
			if err != nil || current == nil || *current != *record {
				return RouteStatus{Readiness: ReadinessUncertain}, ErrUncertain
			}
			status := RouteStatus{Readiness: ReadinessUncertain}
			if _, err := m.readInstalledReservation(record); err != nil {
				return status, err
			}
			if validate != nil {
				if err := validate(); err != nil {
					return status, err
				}
			}
			status, err = m.VerifyRegisteredRoute(ctx, "production", installationID, origin, httpsPort, backendPort)
			current, readErr := m.readRegistration()
			if readErr != nil || current == nil || *current != *record {
				return RouteStatus{Readiness: ReadinessUncertain}, errors.Join(err, ErrUncertain)
			}
			if _, reservationErr := m.readInstalledReservation(record); reservationErr != nil {
				status.Readiness = ReadinessUncertain
				return status, errors.Join(err, reservationErr)
			}
			if validate != nil {
				if validationErr := validate(); validationErr != nil {
					status.Readiness = ReadinessUncertain
					return status, errors.Join(err, validationErr)
				}
			}
			return status, err
		}
		lifetime, retire := context.WithCancel(ctx)
		var sessionMu sync.Mutex
		var checks sync.WaitGroup
		active := true
		defer func() {
			sessionMu.Lock()
			active = false
			retire()
			sessionMu.Unlock()
			checks.Wait() // Ownership cannot escape an in-flight observation.
		}()
		check := func(callCtx context.Context) (status RouteStatus, err error) {
			// Nil means no caller deadline, not exemption from lifetime cancellation.
			if callCtx == nil {
				callCtx = context.Background()
			}
			sessionMu.Lock()
			if !active {
				sessionMu.Unlock()
				return RouteStatus{}, ErrWorkflowRequired
			}
			checks.Add(1)
			sessionMu.Unlock()
			defer checks.Done()
			ctx, cancel := context.WithCancel(callCtx)
			done := make(chan struct{})
			stop := context.AfterFunc(lifetime, func() { defer close(done); cancel() })
			defer func() {
				if !stop() {
					<-done
				}
			}()
			defer cancel()
			if lifetime.Err() != nil {
				cancel()
			}
			if err := ctx.Err(); err != nil {
				return RouteStatus{}, err
			}
			err = m.withOwnedReadOnlyCoordination(ctx, record.NodeID, func() error {
				status, err = checkUnlocked(ctx)
				return err
			})
			err = errors.Join(err, ctx.Err())
			return
		}
		status, err := check(acquire)
		if err != nil {
			return err
		}
		if status.Readiness != ReadinessReady {
			return ErrUncertain
		}
		return run(ctx, check)
	})
}
