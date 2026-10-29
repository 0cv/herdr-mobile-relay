package tailscalecli

import (
	"context"
	"errors"
	"testing"
)

func TestInstalledAcknowledgementCoordinationCancellation(t *testing.T) {
	isolateAcknowledgementFixture(t)
	for _, lock := range []string{"node", "reservation"} {
		t.Run(lock, func(t *testing.T) {
			f := newFakeCLI(t)
			m := newFixtureManager(t, f, "ack-coordination")
			b := installedFixtureBinding(t, m, fixtureRequest(true))
			err := m.WithInstalledSetupSession(context.Background(), b, func(ctx context.Context, s *InstalledSetupSession) error {
				s.observeReadiness = func(context.Context) error { return nil }
				if err := s.BindRuntimeGeneration(ctx, b, "ack-generation"); err != nil {
					return err
				}
				if err := s.Publish(ctx, b, setupConsent(t, s, b), newTestBackendLease(true)); err != nil {
					return err
				}
				return checkInstalledContendedCancellation(t, m, f, lock, func(call context.Context) (RouteStatus, error) {
					v, err := s.AcknowledgedPublication(call, b)
					if v != (AcknowledgedPublication{}) {
						return RouteStatus{}, errors.New("canceled getter leaked data")
					}
					return RouteStatus{}, err
				})
			})
			if err != nil {
				t.Fatal(err)
			}
			if f.mutationCalls() != 1 {
				t.Fatal("getter mutated CLI")
			}
		})
	}
}
