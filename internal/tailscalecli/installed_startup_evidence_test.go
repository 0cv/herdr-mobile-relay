package tailscalecli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestInstalledResumeStartupUnavailableInvalidReservation(t *testing.T) {
	for _, state := range []RegistrationState{"missing", StatePublishPending, StatePublishUncertain, StateRemovePending, StateRemoved} {
		t.Run(string(state), func(t *testing.T) {
			f := newFakeCLI(t)
			m := newFixtureManager(t, f, "closed-startup")
			r := fixtureRequest(true)
			r.Scope, r.Consent.Scope = "production", "production"
			if err := m.Publish(context.Background(), r); err != nil {
				t.Fatal(err)
			}
			binding := installedFixtureBinding(t, m, r)
			if state == "missing" {
				if err := os.Remove(filepath.Join(m.coordinationRoot, fmt.Sprintf("backend-port-%d.json", r.BackendPort))); err != nil {
					t.Fatal(err)
				}
			} else {
				rec, err := m.readBackendReservation(r.BackendPort)
				if err != nil {
					t.Fatal(err)
				}
				rec.State = state
				if err := m.writeBackendReservation(*rec); err != nil {
					t.Fatal(err)
				}
			}
			f.status = `{"BackendState":"Stopped"}`
			before := len(f.calls)
			m.installedStartupPolicy = &installedStartupPolicy{time.Second, func(context.Context, time.Duration) error { t.Fatal("invalid reservation retried"); return nil }}
			err := m.WithInstalledSession(context.Background(), binding, func(context.Context, *InstalledSession) error { t.Fatal("invalid reservation admitted"); return nil })
			if err == nil || len(f.calls) != before {
				t.Fatalf("invalid reservation reached CLI: %v", err)
			}
		})
	}
}
