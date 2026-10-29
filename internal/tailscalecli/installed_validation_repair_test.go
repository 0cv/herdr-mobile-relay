package tailscalecli

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Real owned session serialization stays occupied until after the cancelled
// validation has returned. No waiter is detached, including timeout cleanup.
func TestInstalledSetupValidationContextQueue(t *testing.T) {
	f := newFakeCLI(t)
	m := newFixtureManager(t, f, "validation-queue")
	b := installedFixtureBinding(t, m, fixtureRequest(true))
	err := m.WithInstalledSetupSession(context.Background(), b, func(ctx context.Context, s *InstalledSetupSession) error {
		entered, release, joined := make(chan struct{}), make(chan struct{}), make(chan struct{})
		go func() {
			defer close(joined)
			_ = s.operation(ctx, b, func(context.Context) error { close(entered); <-release; return nil })
		}()
		caller, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		var validationErr error
		started := false
		queued := &setupQueueContext{Context: caller, admitted: make(chan struct{})}
		defer func() {
			cancel()
			close(release)
			<-joined
			if started {
				<-done
			}
		}()
		select {
		case <-entered:
		case <-joined:
			return errors.New("holder did not enter")
		case <-time.After(5 * time.Second):
			return errors.New("holder entry timeout")
		}
		started = true
		go func() { defer close(done); validationErr = s.ValidateRuntimeBindingContext(queued, b) }()
		select {
		case <-queued.admitted:
		case <-done:
			return errors.New("validation never queued")
		case <-time.After(5 * time.Second):
			return errors.New("validation queue timeout")
		}
		cancel()
		select {
		case <-done:
			if !errors.Is(validationErr, context.Canceled) {
				t.Error("validation lost cancellation")
			}
		case <-time.After(5 * time.Second):
			t.Error("validation waited behind held session serialization")
		}
		select {
		case <-joined:
			t.Error("blocker escaped")
		default:
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.mutationCalls() != 0 {
		t.Fatal("validation mutated route")
	}
}

func TestInstalledResumeCheckPreservesCancellationEvidence(t *testing.T) {
	f := newFakeCLI(t)
	m := newFixtureManager(t, f, "cancel-evidence")
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
	checking := false
	err = m.acquireInstalledResume(context.Background(), context.Background(), record, r.InstallationID, r.Origin, r.HTTPSPort, r.BackendPort, func() error {
		if checking {
			cancel()
			return ErrConflict
		}
		return nil
	}, func(_ context.Context, check func(context.Context) (RouteStatus, error)) error {
		checking = true
		_, err := check(call)
		if !errors.Is(err, ErrConflict) || !errors.Is(err, context.Canceled) {
			t.Error("owned check erased conflict/cancellation sibling")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.mutationCalls() != 1 {
		t.Fatal("resume changed route")
	}
}
