package tailscalecli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// All executors are in-memory. Readiness substitutes only observation; the
// actual session reservation, consent, lifetime locks and logical lease remain.
func setupConsent(t *testing.T, s *InstalledSetupSession, b InstalledBinding) Consent {
	t.Helper()
	p, err := s.Proposal(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fmt.Sprintf("%#v", p), p.ReservationID) {
		t.Fatal("proposal formatting leaks attempt")
	}
	c := fixtureConsent(true)
	c.Scope = "production"
	c.ReservationID = p.ReservationID
	c.RouteConfirmation = p.Confirmation
	return c
}
func TestInstalledSetupLifecycle(t *testing.T) {
	f := newFakeCLI(t)
	f.serve = unrelatedRoute
	m := newFixtureManager(t, f, "setup")
	r := fixtureRequest(true)
	b := installedFixtureBinding(t, m, r)
	var retained *InstalledSetupSession
	err := m.WithInstalledSetupSession(context.Background(), b, func(ctx context.Context, s *InstalledSetupSession) error {
		retained = s
		record, e := m.readRegistration()
		if e != nil || record != nil {
			t.Fatal("setup preseeded journal")
		}
		reserved, e := m.readBackendReservation(r.BackendPort)
		if e != nil || reserved == nil || reserved.State != StatePublishPending {
			t.Fatal("missing real reservation")
		}
		lease := newTestBackendLease(true)
		s.observeReadiness = func(context.Context) error {
			if lease.held.Load() != 1 {
				t.Fatal("readiness without lease")
			}
			return nil
		}
		f.publishHook = func() {
			if lease.held.Load() != 1 {
				t.Fatal("dispatch without lease")
			}
		}
		c := setupConsent(t, s, b)
		// Same instance and backend must remain excluded throughout the callback.
		duplicate := *m
		competing, cancel := context.WithCancel(ctx)
		duplicate.fixtureLockWaitHook = func(string) { cancel() }
		if duplicate.WithInstalledSetupSession(competing, b, func(context.Context, *InstalledSetupSession) error { t.Fatal("competing owner"); return nil }) == nil {
			t.Fatal("ownership escaped")
		}
		cancel()
		if err := s.Publish(ctx, b, c, lease); err != nil {
			return err
		}
		if s.Publish(ctx, b, c, newTestBackendLease(true)) == nil {
			t.Fatal("attempt replayed")
		}
		status, err := s.VerifyRegisteredRoute(ctx, b)
		if err != nil || status.Readiness != ReadinessReady {
			t.Fatalf("readback: %+v %v", status, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if retained.ValidateRuntimeBinding(b) == nil {
		t.Fatal("retained authority")
	}
	if _, err := retained.Proposal(context.Background(), b); err == nil {
		t.Fatal("retained proposal")
	}
	if err := m.WithInstalledSetupSession(context.Background(), b, func(context.Context, *InstalledSetupSession) error { t.Fatal("duplicate setup"); return nil }); err == nil {
		t.Fatal("existing record accepted")
	}
	if err := m.WithInstalledSession(context.Background(), b, func(ctx context.Context, s *InstalledSession) error { return s.ValidateRuntimeBinding(b) }); err != nil {
		t.Fatal(err)
	}
	if f.mutationCalls() != 1 || f.serve != unrelatedAndFixtureRoutes {
		t.Fatal("resume mutated or lost unrelated route")
	}
}

func TestInstalledSetupRefusals(t *testing.T) {
	for _, kind := range []string{"binding", "consent", "attempt", "risk", "development", "missing-lease", "inactive-lease", "cancel", "missing-reservation"} {
		t.Run(kind, func(t *testing.T) {
			f := newFakeCLI(t)
			m := newFixtureManager(t, f, "refuse")
			b := installedFixtureBinding(t, m, fixtureRequest(true))
			err := m.WithInstalledSetupSession(context.Background(), b, func(ctx context.Context, s *InstalledSetupSession) error {
				s.observeReadiness = func(context.Context) error { return nil }
				c := setupConsent(t, s, b)
				selected := b
				var lease BackendLease = newTestBackendLease(true)
				switch kind {
				case "binding":
					selected.input.InstallationID = "other"
				case "consent":
					c.Accepted = false
				case "attempt":
					c.ReservationID = "000000000000000000000000000000ff"
				case "risk":
					c.NoRollbackAccepted = false
				case "development":
					c.RouteConfirmation = fixtureConsent(true).RouteConfirmation
				case "missing-lease":
					lease = nil
				case "inactive-lease":
					lease = newTestBackendLease(false)
				case "cancel":
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					cancel()
				case "missing-reservation":
					if err := m.removeBackendReservation(s.reservation); err != nil {
						t.Fatal(err)
					}
				}
				if s.Publish(ctx, selected, c, lease) == nil {
					t.Fatal("refusal accepted")
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if f.mutationCalls() != 0 {
				t.Fatal("refusal dispatched")
			}
		})
	}
	var zero InstalledSetupSession
	if zero.ValidateRuntimeBinding(InstalledBinding{}) == nil {
		t.Fatal("zero handle")
	}
}

func TestInstalledSetupDurableFailures(t *testing.T) {
	for _, kind := range []string{"dispatch", "receipt", "promotion", "readback"} {
		t.Run(kind, func(t *testing.T) {
			f := newFakeCLI(t)
			m := newFixtureManager(t, f, "failure")
			b := installedFixtureBinding(t, m, fixtureRequest(true))
			err := m.WithInstalledSetupSession(context.Background(), b, func(ctx context.Context, s *InstalledSetupSession) error {
				s.observeReadiness = func(context.Context) error { return nil }
				s.beforeDurableWrite = func(stage string) error {
					if stage == kind {
						return errors.New("injected durable failure")
					}
					return nil
				}
				if kind == "dispatch" {
					f.publishErr = errors.New("lost reply")
					f.publishDispatched = true
				}
				if kind == "readback" {
					f.publishNoEffect = true
				}
				c := setupConsent(t, s, b)
				if s.Publish(ctx, b, c, newTestBackendLease(true)) == nil {
					t.Fatal("failure accepted")
				}
				if s.Publish(ctx, b, c, newTestBackendLease(true)) == nil {
					t.Fatal("failure replay accepted")
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			r, err := m.readRegistration()
			if err != nil || r == nil {
				t.Fatal("lost intent")
			}
			reserved, err := m.readBackendReservation(b.input.BackendPort)
			if err != nil || reserved == nil {
				t.Fatal("lost reservation")
			}
			want := StateRegistered
			if kind == "dispatch" {
				want = StatePublishUncertain
			}
			if kind == "receipt" {
				want = StatePublishPending
			}
			if r.State != want {
				t.Fatalf("state %s want %s", r.State, want)
			}
			if kind != "readback" && reserved.State != StatePublishPending {
				t.Fatal("false promotion")
			}
			if f.mutationCalls() != 1 {
				t.Fatal("replayed uncertain dispatch")
			}
		})
	}
}

func TestInstalledSetupRetirementDrains(t *testing.T) {
	f := newFakeCLI(t)
	m := newFixtureManager(t, f, "drain")
	b := installedFixtureBinding(t, m, fixtureRequest(true))
	entered := make(chan struct{})
	done := make(chan error, 1)
	err := m.WithInstalledSetupSession(context.Background(), b, func(ctx context.Context, s *InstalledSetupSession) error {
		s.observeReadiness = func(c context.Context) error { close(entered); <-c.Done(); return c.Err() }
		consent := setupConsent(t, s, b)
		go func() { done <- s.Publish(context.Background(), b, consent, newTestBackendLease(true)) }()
		<-entered
		return nil // retirement must cancel and join publication before unlocking
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("operation outlived owner")
	}
	if f.mutationCalls() != 0 {
		t.Fatal("cancelled readiness dispatched")
	}
}

func TestInstalledSetupProductionClosed(t *testing.T) {
	f := newFakeCLI(t)
	m := newPolicyManager(t, f, "darwin", "arm64", "closed-setup")
	b := installedFixtureBinding(t, m, fixtureRequest(true))
	f.calls = nil
	err := m.WithInstalledSetupSession(context.Background(), b, func(context.Context, *InstalledSetupSession) error { t.Fatal("production entered"); return nil })
	if !errors.Is(err, ErrUnsupported) || len(f.calls) != 0 {
		t.Fatalf("production effects: %v", err)
	}
}
