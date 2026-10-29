package tailscalecli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Change only temporary reservation evidence at the final inert CLI read.
func TestInstalledResumePostObservationReservation(t *testing.T) {
	for _, phase := range []string{"initial", "callback-check"} {
		for _, change := range []string{"missing", "pending", "tuple", "malformed"} {
			t.Run(phase+"/"+change, func(t *testing.T) {
				f := newFakeCLI(t)
				m := newFixtureManager(t, f, "post-observation")
				r := fixtureRequest(true)
				r.Scope, r.Consent.Scope = "production", "production"
				if err := m.Publish(context.Background(), r); err != nil {
					t.Fatal(err)
				}
				journalPath := filepath.Join(m.stateRoot, "registration.json")
				before, err := os.ReadFile(journalPath)
				if err != nil {
					t.Fatal(err)
				}
				reservationPath := filepath.Join(m.coordinationRoot, fmt.Sprintf("backend-port-%d.json", r.BackendPort))
				armed, changed := phase == "initial", false
				var altered []byte
				m.client = newTestClientForPlatform(m.client.binary, func(ctx context.Context, binary string, args ...string) (commandResult, error) {
					result, err := f.run(ctx, binary, args...)
					if armed && !changed && strings.Join(args, " ") == "serve status --json" {
						changed = true
						switch change {
						case "missing":
							if err := os.Remove(reservationPath); err != nil {
								t.Fatal(err)
							}
						case "malformed":
							if err := os.WriteFile(reservationPath, []byte("{"), 0600); err != nil {
								t.Fatal(err)
							}
						default:
							reservation, err := m.readBackendReservation(r.BackendPort)
							if err != nil || reservation == nil {
								t.Fatalf("reservation: %v", err)
							}
							if change == "pending" {
								reservation.State = StatePublishPending
								reservation.ReservationID = "11111111111111111111111111111111"
							} else {
								reservation.InstallationID = "different-install"
							}
							data, err := json.Marshal(reservation)
							if err != nil {
								t.Fatal(err)
							}
							if err := os.WriteFile(reservationPath, data, 0600); err != nil {
								t.Fatal(err)
							}
						}
						if change != "missing" {
							altered, err = os.ReadFile(reservationPath)
							if err != nil {
								t.Fatal(err)
							}
						}
					}
					return result, err
				}, "darwin", "arm64")
				entries := 0
				err = m.WithInstalledResume(context.Background(), r.InstallationID, r.Origin, r.HTTPSPort, r.BackendPort, func(_ context.Context, check func() (RouteStatus, error)) error {
					entries++
					if phase == "initial" {
						return nil
					}
					armed = true
					status, err := check()
					if !errors.Is(err, ErrUncertain) || status.Readiness != ReadinessUncertain {
						t.Errorf("post-observation check: readiness=%s err=%v", status.Readiness, err)
					}
					return err
				})
				wantEntries := 0
				if phase == "callback-check" {
					wantEntries = 1
				}
				if !changed || entries != wantEntries || !errors.Is(err, ErrUncertain) {
					t.Errorf("changed=%v entries=%d want=%d err=%v", changed, entries, wantEntries, err)
				}
				after, readErr := os.ReadFile(journalPath)
				if readErr != nil || string(before) != string(after) {
					t.Fatalf("journal changed: %v", readErr)
				}
				after, readErr = os.ReadFile(reservationPath)
				if change == "missing" {
					if !os.IsNotExist(readErr) {
						t.Fatalf("reservation recreated: %v", readErr)
					}
				} else if readErr != nil || string(after) != string(altered) {
					t.Fatalf("reservation repaired: %v", readErr)
				}
				if f.mutationCalls() != 1 || f.serve != fixtureRoute {
					t.Fatal("resume mutated route")
				}
			})
		}
	}
}
