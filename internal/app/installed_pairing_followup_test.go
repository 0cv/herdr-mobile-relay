//go:build herdr_installed_inert_test

package app

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/deviceauth"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

// Synthetic post-binding phase only, not concrete admission or qualification.
// The real leaf calls this release helper AFTER its filesystem-backed boundary.
func TestInstalledPairingSyntheticFinalGuards(t *testing.T) {
	for _, name := range []string{"terminal", "suspension", "store-changed"} {
		t.Run(name, func(t *testing.T) {
			store := &deviceauth.Store{}
			gate := deviceauth.NewBootstrapGate()
			if err := gate.Attach(store); err != nil {
				t.Fatal(err)
			}
			if err := gate.Open(); err != nil {
				t.Fatal(err)
			}
			s := &Server{deviceAuth: store, bootstrapGate: gate, installedCLI: &tailscalecli.InstalledSession{}}
			payload := installedPairingPayload{URL: "synthetic-payload"}
			if got, err := s.installedPairingRelease(store, payload); err != nil || got != payload {
				t.Fatal("synthetic initial guard not open")
			}
			// Model a transition during final binding reads, without a fake
			// positive owner or a replaceable production validation callback.
			switch name {
			case "terminal":
				s.recordInstalledPairingFailure(tailscalecli.ErrConflict)
			case "suspension":
				gate.SuspendAdmission()
			case "store-changed":
				s.deviceAuthMu.Lock()
				s.deviceAuth = &deviceauth.Store{}
				s.deviceAuthMu.Unlock()
			}
			if got, err := s.installedPairingRelease(store, payload); !errors.Is(err, deviceauth.ErrBootstrapGateClosed) || got.URL != "" {
				t.Fatal("late guard transition released payload")
			}
		})
	}
}

func TestInstalledPairingScopeQueuedCancellation(t *testing.T) {
	for _, name := range []string{"cancel", "retire"} {
		t.Run(name, func(t *testing.T) {
			IsolateInstalledFixture(t)
			resource := &installedLogicalListener{}
			b := newInstalledBackend(context.Background(), resource)
			s := &Server{installedBackend: b}
			unlock, err := s.externalArmMu.Lock(context.Background())
			if err != nil {
				_ = b.Close()
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			waiting, release := make(chan struct{}), make(chan struct{})
			done, closed := make(chan error, 1), make(chan error, 1)
			var entered atomic.Bool
			var releaseOnce sync.Once
			releaseObserver := func() { releaseOnce.Do(func() { close(release) }) }
			operationJoined, closeStarted, closeJoined := false, false, false
			// No assertion/timeout can restore isolation with live owned work.
			defer func() {
				cancel()
				releaseObserver()
				unlock()
				if !operationJoined {
					<-done
				}
				if closeStarted && !closeJoined {
					<-closed
				}
				_ = b.Close()
			}()
			go func() {
				done <- s.withInstalledPairingScope(ctx, func(context.Context) error {
					entered.Store(true)
					return nil
				}, func() {
					// Called by the REAL lock's select evaluation, after lease
					// entry, not by context propagation or a terminal leaf guard.
					close(waiting)
					if name == "retire" {
						<-release
					}
				})
			}()
			select {
			case <-waiting:
			case <-time.After(2 * time.Second):
				t.Fatal("scope did not reach held lock")
			}
			if resource.closed.Load() || b.lifetime.Err() != nil {
				t.Fatal("queue did not have active backend")
			}
			cancel() // externalArmMu remains held through all assertions below.
			if name == "retire" {
				closeStarted = true
				go func() { closed <- b.Close() }()
				select {
				case <-b.lifetime.Done():
				case <-time.After(2 * time.Second):
					t.Fatal("close did not retire backend")
				}
				if resource.closed.Load() {
					t.Fatal("physical close preceded queued lease retirement")
				}
				select {
				case <-closed:
					closeJoined = true
					t.Fatal("close returned with queued lease held")
				default:
				}
				releaseObserver()
			}
			select {
			case err := <-done:
				operationJoined = true
				if !errors.Is(err, context.Canceled) || entered.Load() {
					t.Fatal("cancelled queued scope entered operation")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("queue cancellation waited for externalArmMu")
			}
			if !closeStarted {
				closeStarted = true
				go func() { closed <- b.Close() }()
			}
			select {
			case err := <-closed:
				closeJoined = true
				if err != nil || !resource.closed.Load() {
					t.Fatal("retired lease did not permit physical close")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("queued scope leaked backend lease")
			}
		})
	}
}
