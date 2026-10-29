package tailscalecli

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// Private file/flock and in-memory CLI evidence only, not app admission.
func TestInstalledOwnedReadOnlyLockCancellation(t *testing.T) {
	for _, mode := range []string{"setup", "resume"} {
		for _, lock := range []string{"node", "reservation"} {
			t.Run(mode+"/"+lock, func(t *testing.T) {
				f := newFakeCLI(t)
				m := newFixtureManager(t, f, "readonly-cancel")
				r := fixtureRequest(true)
				b := installedFixtureBinding(t, m, r)
				exercise := func(check func(context.Context) (RouteStatus, error)) error {
					return checkInstalledContendedCancellation(t, m, f, lock, check)
				}
				var err error
				if mode == "setup" {
					err = m.WithInstalledSetupSession(context.Background(), b, func(ctx context.Context, s *InstalledSetupSession) error {
						s.observeReadiness = func(context.Context) error { return nil }
						if err := s.Publish(ctx, b, setupConsent(t, s, b), newTestBackendLease(true)); err != nil {
							return err
						}
						return exercise(func(call context.Context) (RouteStatus, error) { return s.VerifyRegisteredRoute(call, b) })
					})
				} else {
					r.Scope, r.Consent.Scope = "production", "production"
					if err = m.Publish(context.Background(), r); err != nil {
						t.Fatal(err)
					}
					err = m.withInstalledResume(context.Background(), r.InstallationID, r.Origin, r.HTTPSPort, r.BackendPort, func(_ context.Context, check func(context.Context) (RouteStatus, error)) error {
						return exercise(check)
					})
				}
				if err != nil {
					t.Fatal(err)
				}
				if f.mutationCalls() != 1 {
					t.Fatal("read-only cancellation mutated route")
				}
			})
		}
	}
}

func checkInstalledContendedCancellation(t *testing.T, m *Manager, f *fakeCLI, lock string, check func(context.Context) (RouteStatus, error)) error {
	t.Helper()
	held, release, holderDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	contended := make(chan struct{})
	var once sync.Once
	m.fixtureLockWaitHook = func(string) { once.Do(func() { close(contended) }) }
	holderCtx, stopHolder := context.WithCancel(context.Background())
	call, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var holderErr, requestErr error
	started := false
	defer func() {
		cancel()
		stopHolder()
		close(release)
		<-holderDone
		if started {
			<-done
		}
		m.fixtureLockWaitHook = nil
	}()
	go func() {
		defer close(holderDone)
		body := func() error { close(held); <-release; return nil }
		if lock == "node" {
			holderErr = m.withNodeLock(holderCtx, "node-fixture", body)
		} else {
			holderErr = m.withBackendReservationLock(holderCtx, body)
		}
	}()
	select {
	case <-held:
	case <-holderDone:
		return errors.Join(errors.New("holder failed"), holderErr)
	case <-time.After(5 * time.Second):
		return errors.New("holder timeout")
	}
	f.mu.Lock()
	before := len(f.calls)
	f.mu.Unlock()
	started = true
	go func() { defer close(done); _, requestErr = check(call) }()
	select {
	case <-contended:
	case <-done:
		return errors.New("request did not contend")
	case <-time.After(5 * time.Second):
		return errors.New("contention timeout")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		return errors.New("request did not finish while holder remained held")
	}
	if !errors.Is(requestErr, context.Canceled) || errors.Is(requestErr, ErrUncertain) || errors.Is(requestErr, ErrConflict) {
		return errors.New("known-unentered cancellation manufactured terminal evidence")
	}
	select {
	case <-holderDone:
		return errors.New("holder released early")
	default:
	}
	f.mu.Lock()
	after := len(f.calls)
	f.mu.Unlock()
	if after != before {
		return errors.New("protected route observation entered")
	}
	return nil
}

func TestInstalledOwnedReadOnlyEnteredCancellationEvidence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		evidence error
	}{{"conflict", ErrConflict}, {"uncertain", ErrUncertain}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeCLI(t)
			m := newFixtureManager(t, f, "readonly-entered")
			r := fixtureRequest(true)
			r.Scope, r.Consent.Scope = "production", "production"
			if err := m.Publish(context.Background(), r); err != nil {
				t.Fatal(err)
			}
			record, err := m.readRegistration()
			if err != nil {
				t.Fatal(err)
			}
			call, cancel := context.WithCancel(context.Background())
			defer cancel()
			checking, entered := false, false
			err = m.acquireInstalledResume(context.Background(), context.Background(), record, r.InstallationID, r.Origin, r.HTTPSPort, r.BackendPort, func() error {
				if checking {
					entered = true
					cancel()
					return tc.evidence
				}
				return nil
			}, func(_ context.Context, check func(context.Context) (RouteStatus, error)) error {
				checking = true
				_, err := check(call)
				if !entered || !errors.Is(err, tc.evidence) || !errors.Is(err, context.Canceled) {
					return errors.New("entered evidence/cancellation lost")
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if f.mutationCalls() != 1 {
				t.Fatal("resume mutated route")
			}
		})
	}
}
