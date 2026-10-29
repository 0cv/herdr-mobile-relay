package tailscalecli

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Only inert command runners and private temporary flock files are used.
func TestInstalledResumeCallCancellation(t *testing.T) {
	for _, mode := range []string{"caller", "deadline", "service", "retirement", "lock"} {
		t.Run(mode, func(t *testing.T) {
			f := newFakeCLI(t)
			m := newFixtureManager(t, f, "cancel")
			r := fixtureRequest(true)
			r.Scope, r.Consent.Scope = "production", "production"
			if err := m.Publish(context.Background(), r); err != nil {
				t.Fatal(err)
			}
			lifetime, stop := context.WithCancel(context.Background())
			defer stop()
			var retained *InstalledSession
			var done chan error
			var observedExit chan struct{}
			err := m.WithInstalledSession(lifetime, installedFixtureBinding(t, m, r), func(ctx context.Context, s *InstalledSession) error {
				retained = s
				verify := func(ctx context.Context) error {
					_, err := s.VerifyRegisteredRoute(ctx, "production", r.InstallationID, r.Origin, r.HTTPSPort, r.BackendPort)
					return err
				}
				if err := verify(nil); err != nil {
					t.Fatal(err)
				}
				call, cancel := context.WithCancel(context.Background())
				defer cancel()
				if mode == "retirement" {
					// Retirement, not the caller defer, must stop the observation.
					call = context.Background()
				}
				if mode == "deadline" {
					var deadlineCancel context.CancelFunc
					call, deadlineCancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
					defer deadlineCancel()
				}
				started := make(chan struct{})
				done = make(chan error, 1)
				if mode == "lock" {
					m.fixtureLockWaitHook = func(string) { cancel() }
					defer func() { m.fixtureLockWaitHook = nil }()
					return m.withNodeLock(ctx, "node-fixture", func() error {
						err := verify(call)
						if !errors.Is(err, context.Canceled) {
							t.Fatalf("lock cancellation: %v", err)
						}
						if lifetime.Err() != nil {
							t.Fatal("lifetime canceled")
						}
						return nil
					})
				}
				calls := 0
				observedExit = make(chan struct{})
				m.client.run = func(ctx context.Context, _ string, _ ...string) (commandResult, error) {
					calls++
					if calls != 1 {
						t.Error("additional observation after cancellation")
					}
					if mode == "deadline" {
						actual, ok := ctx.Deadline()
						expected, _ := call.Deadline()
						if !ok || actual.After(expected) {
							t.Error("deadline inflated")
						}
					}
					close(started)
					<-ctx.Done()
					close(observedExit)
					return commandResult{}, ctx.Err()
				}
				go func() { done <- verify(call) }()
				<-started
				if mode == "retirement" {
					return nil
				}
				if mode == "service" {
					stop()
				} else if mode != "deadline" {
					cancel()
				}
				select {
				case err := <-done:
					if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("cancellation: %v", err)
					}
				case <-time.After(time.Second):
					t.Fatal("check ignored cancellation")
				}
				if mode != "service" && lifetime.Err() != nil {
					t.Fatal("lifetime canceled")
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if mode == "retirement" {
				select {
				case <-observedExit:
				default:
					t.Fatal("ownership returned before observation drained")
				}
				select {
				case err := <-done:
					if !errors.Is(err, context.Canceled) {
						t.Fatal(err)
					}
				case <-time.After(time.Second):
					t.Fatal("retired check did not return")
				}
			}
			if _, err := retained.VerifyRegisteredRoute(nil, "production", r.InstallationID, r.Origin, r.HTTPSPort, r.BackendPort); !errors.Is(err, ErrWorkflowRequired) {
				t.Fatal(err)
			}
			if f.mutationCalls() != 1 {
				t.Fatal("resume mutated route")
			}
		})
	}
}
