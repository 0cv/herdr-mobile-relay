package tailscalecli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestInstalledAcknowledgementDurableRefusals(t *testing.T) {
	isolateAcknowledgementFixture(t)
	for _, kind := range []string{"missing-record", "read-error", "pending-record", "changed-record", "foreign-record", "missing-reservation", "changed-reservation", "invalid-generation", "post-unlock-error", "final-binding-cancel-siblings"} {
		t.Run(kind, func(t *testing.T) {
			f := newFakeCLI(t)
			m := newFixtureManager(t, f, "ack-refusal")
			b := installedFixtureBinding(t, m, fixtureRequest(true))
			fault := errors.New("post-real-unlock fault")
			err := m.WithInstalledSetupSession(context.Background(), b, func(ctx context.Context, s *InstalledSetupSession) error {
				s.observeReadiness = func(context.Context) error { return nil }
				if err := s.BindRuntimeGeneration(ctx, b, "ack-generation"); err != nil {
					return err
				}
				if err := s.Publish(ctx, b, setupConsent(t, s, b), newTestBackendLease(true)); err != nil {
					return err
				}
				call, cancel := context.WithCancel(ctx)
				defer cancel()
				r := *s.receipt
				switch kind {
				case "missing-record":
					if err := os.Remove(filepath.Join(m.stateRoot, journalName)); err != nil {
						return err
					}
				case "read-error":
					if err := os.WriteFile(filepath.Join(m.stateRoot, journalName), []byte("not json"), 0600); err != nil {
						return err
					}
				case "pending-record":
					r.State = StatePublishPending
					r.MutationAcknowledged = false
					r.ReservationID = s.reservation.ReservationID
					if err := m.writeRegistration(r); err != nil {
						return err
					}
				case "changed-record":
					r.UpdatedAt = "2000-01-01T00:00:00Z"
					if err := m.writeRegistration(r); err != nil {
						return err
					}
				case "foreign-record":
					r.InstallationID = "foreign"
					if err := m.writeRegistration(r); err != nil {
						return err
					}
				case "missing-reservation":
					if err := m.removeBackendReservation(*s.acknowledged); err != nil {
						return err
					}
				case "changed-reservation":
					reserved := *s.acknowledged
					reserved.UpdatedAt = "2000-01-01T00:00:00Z"
					if reserved.UpdatedAt == s.acknowledged.UpdatedAt {
						reserved.UpdatedAt = "2000-01-02T00:00:00Z"
					}
					if err := m.writeBackendReservation(reserved); err != nil {
						return err
					}
					persisted, err := m.readBackendReservation(reserved.BackendPort)
					if err != nil {
						return err
					}
					if persisted == nil || *persisted != reserved || *persisted == *s.acknowledged || !sameBackendReservation(*persisted, *s.acknowledged) {
						return errors.New("reservation timestamp drift did not persist independently of retained acknowledgement")
					}
				case "invalid-generation":
					s.runtimeGeneration = "invalid/generation"
				case "post-unlock-error":
					s.afterAcknowledgementCoordination = func() error { return fault }
				case "final-binding-cancel-siblings":
					if err := os.MkdirAll(b.input.RuntimeRoot, 0700); err != nil {
						return err
					}
					s.afterAcknowledgementCoordination = func() error {
						cancel()
						return errors.Join(fault, os.Chmod(b.input.RuntimeRoot, 0777))
					}
					defer os.Chmod(b.input.RuntimeRoot, 0700)
				}
				v, err := s.AcknowledgedPublication(call, b)
				if err == nil || v != (AcknowledgedPublication{}) {
					return errors.New("failed getter returned evidence")
				}
				if kind == "post-unlock-error" || kind == "final-binding-cancel-siblings" {
					if !errors.Is(err, fault) {
						return errors.New("lost unlock sibling")
					}
				}
				if kind == "final-binding-cancel-siblings" && (!errors.Is(err, context.Canceled) || !errors.Is(err, ErrPermissionDenied)) {
					return errors.New("lost final validation/cancel sibling")
				}
				return nil
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
