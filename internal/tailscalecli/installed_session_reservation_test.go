package tailscalecli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestInstalledResumeReservationEvidence(t *testing.T) {
	cases := []struct {
		name   string
		change func(*backendPortReservation)
	}{
		{"missing", nil}, {"malformed", nil},
		{"wrong-installation", func(r *backendPortReservation) { r.InstallationID = "other" }},
		{"wrong-node", func(r *backendPortReservation) { r.NodeID = "other" }},
		{"wrong-scope", func(r *backendPortReservation) { r.Scope = "development" }},
		{"wrong-https", func(r *backendPortReservation) { r.HTTPSPort++ }},
		{"wrong-backend", func(r *backendPortReservation) { r.BackendPort++ }},
		{"wrong-origin", func(r *backendPortReservation) { r.Origin = "https://other.tailnet.ts.net:8443" }},
		{"attempt", func(r *backendPortReservation) { r.ReservationID = "00000000000000000000000000000001" }},
		{"malformed-attempt", func(r *backendPortReservation) { r.ReservationID = "bad" }},
		{"interrupted-promotion", func(r *backendPortReservation) {
			r.State = StatePublishPending
			r.ReservationID = "00000000000000000000000000000001"
		}},
	}
	for _, state := range []RegistrationState{StateUnconfigured, StatePublishPending, StatePublishUncertain, StateRemovePending, StateRemoveUncertain, StateRemoved, StateReconciledPresent, StateReconciledAbsent, "invalid"} {
		state := state
		cases = append(cases, struct {
			name   string
			change func(*backendPortReservation)
		}{string(state), func(r *backendPortReservation) { r.State = state }})
	}
	for _, tc := range cases {
		for _, during := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/during=%v", tc.name, during), func(t *testing.T) {
				f := newFakeCLI(t)
				m := newFixtureManager(t, f, "reservation")
				r := fixtureRequest(true)
				r.Scope = "production"
				r.Consent.Scope = "production"
				if err := m.Publish(context.Background(), r); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(m.coordinationRoot, fmt.Sprintf("backend-port-%d.json", r.BackendPort))
				journal, _ := os.ReadFile(filepath.Join(m.stateRoot, journalName))
				var evidence []byte
				corrupt := func() {
					reservation, err := m.readBackendReservation(r.BackendPort)
					if err != nil {
						t.Fatal(err)
					}
					switch tc.name {
					case "missing":
						if err := os.Remove(path); err != nil {
							t.Fatal(err)
						}
						return
					case "malformed":
						evidence = []byte("{")
					default:
						tc.change(reservation)
						evidence, err = json.Marshal(reservation)
						if err != nil {
							t.Fatal(err)
						}
					}
					if err := os.WriteFile(path, evidence, 0600); err != nil {
						t.Fatal(err)
					}
				}
				if !during {
					corrupt()
				}
				entered := false
				err := m.WithInstalledResume(context.Background(), r.InstallationID, r.Origin, r.HTTPSPort, r.BackendPort, func(ctx context.Context, check func() (RouteStatus, error)) error {
					entered = true
					if !during {
						t.Fatal("invalid evidence admitted")
					}
					corrupt()
					status, err := check()
					if err == nil || status.Readiness != ReadinessUncertain {
						t.Fatalf("drift admitted: %+v %v", status, err)
					}
					return err
				})
				if err == nil || entered != during {
					t.Fatalf("unexpected admission: %v %v", entered, err)
				}
				after, readErr := os.ReadFile(path)
				if tc.name == "missing" {
					if !os.IsNotExist(readErr) {
						t.Fatal("missing evidence recreated")
					}
				} else if readErr != nil || string(after) != string(evidence) {
					t.Fatal("reservation repaired")
				}
				afterJournal, _ := os.ReadFile(filepath.Join(m.stateRoot, journalName))
				if string(journal) != string(afterJournal) || f.mutationCalls() != 1 {
					t.Fatal("resume mutated route/journal")
				}
			})
		}
	}
}

func TestInstalledResumeJournalAttemptRefused(t *testing.T) {
	f := newFakeCLI(t)
	m := newFixtureManager(t, f, "journal-attempt")
	r := fixtureRequest(true)
	r.Scope = "production"
	r.Consent.Scope = "production"
	if err := m.Publish(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	record, _ := m.readRegistration()
	record.ReservationID = "00000000000000000000000000000001"
	if err := m.writeRegistration(*record); err != nil {
		t.Fatal(err)
	}
	err := m.WithInstalledResume(context.Background(), r.InstallationID, r.Origin, r.HTTPSPort, r.BackendPort, func(context.Context, func() (RouteStatus, error)) error { t.Fatal("attempt admitted"); return nil })
	after, _ := m.readRegistration()
	if err == nil || *after != *record || f.mutationCalls() != 1 {
		t.Fatal("journal attempt repaired/admitted")
	}
}
