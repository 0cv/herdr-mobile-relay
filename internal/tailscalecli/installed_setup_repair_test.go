package tailscalecli

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// Done is consulted only after operation admission. This handshake avoids sleeps
// when placing a proposal behind the blocked in-memory readiness observation.
type setupQueueContext struct {
	context.Context
	once     sync.Once
	admitted chan struct{}
}

func (c *setupQueueContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.admitted) })
	return c.Context.Done()
}

func TestInstalledSetupQueuedCancellation(t *testing.T) {
	for _, kind := range []string{"caller", "lifetime"} {
		t.Run(kind, func(t *testing.T) {
			f := newFakeCLI(t)
			m := newFixtureManager(t, f, "queue")
			b := installedFixtureBinding(t, m, fixtureRequest(true))
			lifetime, stopLifetime := context.WithCancel(context.Background())
			defer stopLifetime()
			var retained *InstalledSetupSession
			err := m.WithInstalledSetupSession(lifetime, b, func(ctx context.Context, s *InstalledSetupSession) error {
				retained = s
				entered, release := make(chan struct{}), make(chan struct{})
				activeDone := make(chan error, 1)
				s.observeReadiness = func(c context.Context) error {
					close(entered)
					<-release // deliberately delay joining even after cancellation
					return c.Err()
				}
				consent := setupConsent(t, s, b)
				active, stopActive := context.WithCancel(ctx)
				go func() { activeDone <- s.Publish(active, b, consent, newTestBackendLease(true)) }()
				// Always release and join owned work before callback retirement, including Fatal.
				var queuedDone chan struct{}
				defer func() {
					stopActive()
					close(release)
					<-activeDone
					if queuedDone != nil {
						<-queuedDone
					}
				}()
				<-entered
				caller, stopCaller := context.WithCancel(context.Background())
				queued := &setupQueueContext{Context: caller, admitted: make(chan struct{})}
				queuedDone = make(chan struct{})
				var queuedErr error
				go func() { _, queuedErr = s.Proposal(queued, b); close(queuedDone) }()
				defer func() {
					stopCaller()
				}()
				<-queued.admitted
				if kind == "caller" {
					stopCaller()
				} else {
					stopLifetime()
				}
				select {
				case <-queuedDone:
					if !errors.Is(queuedErr, context.Canceled) {
						t.Errorf("queued result: %v", queuedErr)
					}
				case <-time.After(5 * time.Second):
					t.Error("cancelled proposal waited for unrelated active observation")
				}
				// Ownership must still exclude another owner while readiness is unjoined.
				duplicate := *m
				competing, cancel := context.WithCancel(context.Background())
				defer cancel()
				duplicate.fixtureLockWaitHook = func(string) { cancel() }
				if err := duplicate.WithInstalledSetupSession(competing, b, func(context.Context, *InstalledSetupSession) error {
					t.Error("active operation lost lifetime ownership")
					return nil
				}); err == nil {
					t.Error("competing owner accepted")
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if retained.ValidateRuntimeBinding(b) != ErrWorkflowRequired {
				t.Fatal("retained authority")
			}
			if f.mutationCalls() != 0 {
				t.Fatal("cancelled active operation dispatched")
			}
		})
	}
}

func TestInstalledSetupFunnelPreflight(t *testing.T) {
	for _, value := range []string{"true", "false"} {
		t.Run(value, func(t *testing.T) {
			f := newFakeCLI(t)
			f.serve = strings.TrimSuffix(unrelatedRoute, "}") + `,"AllowFunnel":{"other.tailnet.ts.net:443":` + value + `}}`
			m := newFixtureManager(t, f, "funnel")
			b := installedFixtureBinding(t, m, fixtureRequest(true))
			called := false
			err := m.WithInstalledSetupSession(context.Background(), b, func(context.Context, *InstalledSetupSession) error { called = true; return nil })
			if !errors.Is(err, ErrConflict) || called {
				t.Fatalf("preflight: %v callback=%v", err, called)
			}
			r, err := m.readRegistration()
			if err != nil || r != nil {
				t.Fatal("preflight created journal", err)
			}
			reservation, err := m.readBackendReservation(b.input.BackendPort)
			if err != nil || reservation != nil {
				t.Fatal("preflight reserved backend", err)
			}
			if f.mutationCalls() != 0 {
				t.Fatal("preflight mutated Serve")
			}
		})
	}
}

func TestInstalledSetupCancelledDispatchedFailure(t *testing.T) {
	f := newFakeCLI(t)
	m := newFixtureManager(t, f, "cancel-dispatch")
	b := installedFixtureBinding(t, m, fixtureRequest(true))
	err := m.WithInstalledSetupSession(context.Background(), b, func(ctx context.Context, s *InstalledSetupSession) error {
		s.observeReadiness = func(context.Context) error { return nil }
		c := setupConsent(t, s, b)
		call, cancel := context.WithCancel(ctx)
		defer cancel()
		var dispatchedReservation backendPortReservation
		f.publishHook = func() {
			reservation, err := m.readBackendReservation(b.input.BackendPort)
			if err != nil || reservation == nil {
				t.Fatal("missing dispatch reservation", err)
			}
			// The transaction refreshes UpdatedAt before dispatch; all other
			// fields must still identify the original pending tuple and attempt.
			original := s.reservation
			original.UpdatedAt = reservation.UpdatedAt
			if reservation.State != StatePublishPending || reservation.ReservationID == "" || *reservation != original {
				t.Fatal("changed pending dispatch attempt")
			}
			dispatchedReservation = *reservation
			cancel()
		}
		f.publishErr = errors.New("private lost reply")
		f.publishDispatched = true
		err := s.Publish(call, b, c, newTestBackendLease(true))
		if !errors.Is(err, ErrUncertain) || !errors.Is(err, context.Canceled) {
			t.Fatalf("lost classification: %v", err)
		}
		if strings.Contains(err.Error(), "private lost reply") {
			t.Fatal("raw command error leaked")
		}
		r, err := m.readRegistration()
		if err != nil || r == nil || r.State != StatePublishUncertain || r.MutationAcknowledged {
			t.Fatal("lost uncertainty", err)
		}
		reservation, err := m.readBackendReservation(b.input.BackendPort)
		if err != nil || reservation == nil || *reservation != dispatchedReservation || r.ReservationID != reservation.ReservationID {
			t.Fatal("lost pending attempt", err)
		}
		// Fresh uncancelled caller must still refuse, not merely fail due to cancellation.
		if err := s.Publish(ctx, b, c, newTestBackendLease(true)); !errors.Is(err, ErrUncertain) {
			t.Fatalf("replay result: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.mutationCalls() != 1 {
		t.Fatal("dispatch count / replay")
	}
}
