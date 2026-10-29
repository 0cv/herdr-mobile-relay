package tailscalecli

import (
	"context"
	"testing"
)

func TestInstalledResumeRetainsAmbiguousJournal(t *testing.T) {
	f := newFakeCLI(t)
	m := newFixtureManager(t, f, "ambiguous")
	r := fixtureRequest(true)
	r.Scope, r.Consent.Scope = "production", "production"
	if err := m.Publish(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	record, err := m.readRegistration()
	if err != nil {
		t.Fatal(err)
	}
	record.State = StatePublishUncertain
	record.MutationAcknowledged = false
	if err := m.writeRegistration(*record); err != nil {
		t.Fatal(err)
	}
	calls := len(f.calls)
	err = m.WithInstalledResume(context.Background(), r.InstallationID, r.Origin, r.HTTPSPort, r.BackendPort,
		func(context.Context, func() (RouteStatus, error)) error {
			t.Fatal("ambiguous route admitted")
			return nil
		})
	if err == nil || len(f.calls) != calls {
		t.Fatalf("ambiguous resume contacted CLI or succeeded: %v", err)
	}
	after, err := m.readRegistration()
	if err != nil || after == nil || *after != *record {
		t.Fatalf("recovery evidence changed: %v", err)
	}
}
