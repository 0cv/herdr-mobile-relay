package tailscalecli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestInstalledResumeStartupWait(t *testing.T) {
	for _, mode := range []string{"ready", "cancel", "journal", "reservation", "missing", "pending", "binding", "terminal"} {
		t.Run(mode, func(t *testing.T) {
			f := newFakeCLI(t)
			m := newFixtureManager(t, f, "startup")
			r := fixtureRequest(true)
			r.Scope, r.Consent.Scope = "production", "production"
			if err := m.Publish(context.Background(), r); err != nil {
				t.Fatal(err)
			}
			binding := installedFixtureBinding(t, m, r)
			f.status = `{"BackendState":"Starting"}`
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var delays []time.Duration
			callsAtCancel := 0
			m.installedStartupPolicy = &installedStartupPolicy{time.Second, func(ctx context.Context, d time.Duration) error {
				delays = append(delays, d)
				// All four lock levels must be available while the scheduler is waiting.
				lockCtx, stop := context.WithTimeout(ctx, time.Second)
				defer stop()
				if err := m.withLifetimeOwnership(lockCtx, r.BackendPort, func() error {
					return m.withNodeLock(lockCtx, "node-fixture", func() error { return m.withBackendReservationLock(lockCtx, func() error { return nil }) })
				}); err != nil {
					t.Fatal(err)
				}
				switch mode {
				case "cancel":
					callsAtCancel = len(f.calls)
					cancel()
					return ctx.Err()
				case "journal":
					rec, err := m.readRegistration()
					if err != nil {
						t.Fatal(err)
					}
					rec.OperationID = "11111111111111111111111111111111"
					if err := m.writeRegistration(*rec); err != nil {
						t.Fatal(err)
					}
				case "reservation", "pending":
					rec, err := m.readBackendReservation(r.BackendPort)
					if err != nil {
						t.Fatal(err)
					}
					if mode == "pending" {
						rec.State = StatePublishPending
						rec.ReservationID = "11111111111111111111111111111111"
					} else {
						rec.UpdatedAt = "2020-01-01T00:00:00Z"
					}
					if err := m.writeBackendReservation(*rec); err != nil {
						t.Fatal(err)
					}
				case "missing":
					if err := os.Remove(filepath.Join(m.coordinationRoot, fmt.Sprintf("backend-port-%d.json", r.BackendPort))); err != nil {
						t.Fatal(err)
					}
				case "binding":
					if err := os.MkdirAll(binding.input.RuntimeRoot, 0777); err != nil {
						t.Fatal(err)
					}
					if err := os.Chmod(binding.input.RuntimeRoot, 0777); err != nil {
						t.Fatal(err)
					}
				case "terminal":
					f.status = `{"BackendState":"NeedsLogin"}`
					return nil
				}
				if mode != "ready" || len(delays) == 8 {
					f.status = fixtureStatus
				} else {
					f.status = `{"BackendState":"Stopped"}`
				}
				return nil
			}}
			entries := 0
			var retained *InstalledSession
			err := m.WithInstalledSession(ctx, binding, func(lifetime context.Context, s *InstalledSession) error {
				entries++
				retained = s
				if _, ok := lifetime.Deadline(); ok {
					t.Fatal("acquisition deadline leaked")
				}
				if lifetime.Err() != nil {
					t.Fatal("attempt cancellation leaked")
				}
				return ErrTransientUnavailable // Must not restart the callback.
			})
			if mode == "ready" {
				want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, time.Minute, time.Minute}
				if entries != 1 || !errors.Is(err, ErrTransientUnavailable) || !reflect.DeepEqual(delays, want) {
					t.Fatalf("entries=%d delays=%v err=%v", entries, delays, err)
				}
				if retained.ValidateRuntimeBinding(binding) == nil {
					t.Fatal("retained authority")
				}
			} else {
				if err == nil || entries != 0 || len(delays) != 1 {
					t.Fatalf("entries=%d delays=%v err=%v", entries, delays, err)
				}
				if mode == "cancel" && (err != context.Canceled || len(f.calls) != callsAtCancel) {
					t.Fatal("observation after cancel")
				}
			}
			if f.mutationCalls() != 1 {
				t.Fatal("startup mutated route")
			}
		})
	}
}

func TestInstalledResumeStartupClassification(t *testing.T) {
	for _, terminal := range []error{ErrConflict, ErrUncertain, ErrLoggedOut, ErrPermissionDenied, ErrUnsupported, ErrInvalidJSON, ErrProfileUnavailable, ErrCommandFailed, errors.New("unknown"), context.Canceled, context.DeadlineExceeded} {
		for _, err := range []error{terminal, errors.Join(ErrTransientUnavailable, terminal), fmt.Errorf("wrapped: %w", errors.Join(ErrTransientUnavailable, terminal))} {
			if onlyError(err, ErrTransientUnavailable) {
				t.Fatalf("retried %v", err)
			}
		}
		sanitized := sanitizeCommandError("status", errors.Join(ErrUncertain, terminal))
		if terminal != ErrUncertain && onlyError(sanitized, ErrTransientUnavailable) {
			t.Fatalf("sanitizer erased terminal %v", terminal)
		}
	}
	if !onlyError(sanitizeCommandError("status", ErrUncertain), ErrTransientUnavailable) {
		t.Fatal("incomplete observation not retryable")
	}
}

func TestInstalledResumeStartupAttemptDeadline(t *testing.T) {
	f := newFakeCLI(t)
	m := newFixtureManager(t, f, "bounded")
	r := fixtureRequest(true)
	r.Scope, r.Consent.Scope = "production", "production"
	if err := m.Publish(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	binding := installedFixtureBinding(t, m, r)
	m.installedStartupPolicy = &installedStartupPolicy{time.Second, func(context.Context, time.Duration) error { t.Fatal("timeout retried"); return nil }}
	// A competing owner cannot be treated as VPN unavailability.
	err := m.withLifetimeOwnership(context.Background(), r.BackendPort, func() error {
		return m.WithInstalledSession(context.Background(), binding, func(context.Context, *InstalledSession) error { t.Fatal("competing owner entered"); return nil })
	})
	if !errors.Is(err, ErrUncertain) {
		t.Fatalf("unbounded acquisition: %v", err)
	}
	original := m.client
	observations := 0
	m.client = newTestClientForPlatform(original.binary, func(ctx context.Context, _ string, _ ...string) (commandResult, error) {
		observations++
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("unbounded initial CLI")
		}
		<-ctx.Done()
		return commandResult{}, ctx.Err()
	}, "darwin", "arm64")
	err = m.WithInstalledSession(context.Background(), binding, func(context.Context, *InstalledSession) error { t.Fatal("timed out observation admitted"); return nil })
	if !errors.Is(err, context.DeadlineExceeded) || observations != 1 {
		t.Fatalf("initial observation bound: %v calls=%d", err, observations)
	}
	m.client = original
	// Allow filesystem preflight a realistic budget, also on loaded test hosts.
	m.installedStartupPolicy.attempt = time.Second
	err = m.WithInstalledSession(context.Background(), binding, func(ctx context.Context, _ *InstalledSession) error {
		// Hold the inert callback beyond the former acquisition deadline.
		time.Sleep(m.installedStartupPolicy.attempt + 50*time.Millisecond)
		if _, ok := ctx.Deadline(); ok || ctx.Err() != nil {
			t.Fatal("attempt owns callback")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
