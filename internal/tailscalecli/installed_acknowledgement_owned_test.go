package tailscalecli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestInstalledAcknowledgementOwnedValues(t *testing.T) {
	isolateAcknowledgementFixture(t)
	for _, session := range []*InstalledSetupSession{nil, {}} {
		if v, err := session.AcknowledgedPublication(context.Background(), InstalledBinding{}); err == nil || v != (AcknowledgedPublication{}) {
			t.Fatal("zero session returned data")
		}
	}
	f := newFakeCLI(t)
	m := newFixtureManager(t, f, "ack-values")
	b := installedFixtureBinding(t, m, fixtureRequest(true))
	var retained *InstalledSetupSession
	err := m.WithInstalledSetupSession(context.Background(), b, func(ctx context.Context, s *InstalledSetupSession) error {
		retained = s
		refuse := func(c context.Context, binding InstalledBinding) {
			t.Helper()
			v, e := s.AcknowledgedPublication(c, binding)
			if e == nil || v != (AcknowledgedPublication{}) {
				t.Fatal("refusal leaked data")
			}
		}
		refuse(ctx, b)
		s.observeReadiness = func(context.Context) error { return nil }
		if err := s.BindRuntimeGeneration(ctx, b, "ack-generation"); err != nil {
			return err
		}
		if err := s.Publish(ctx, b, setupConsent(t, s, b), newTestBackendLease(true)); err != nil {
			return err
		}
		v, err := s.AcknowledgedPublication(ctx, b)
		if err != nil {
			return err
		}
		r, err := m.readRegistration()
		if err != nil {
			return err
		}
		reservation, err := m.readInstalledReservation(r)
		if err != nil {
			return err
		}
		rb, _ := json.Marshal(r)
		bb, _ := json.Marshal(reservation)
		fingerprint, err := b.Fingerprint()
		if err != nil {
			return err
		}
		want := PublicationSnapshot{Selection: fingerprint, RegistrationDigest: sha256.Sum256(rb), ReservationDigest: sha256.Sum256(bb), Installation: b.input.InstallationID, Node: r.NodeID, Profile: string(r.Profile), Origin: b.input.Origin, Attempt: s.reservation.ReservationID, Operation: r.OperationID, Generation: "ack-generation", HTTPSPort: b.input.HTTPSPort, BackendPort: b.input.BackendPort}
		if v.Snapshot() != want {
			t.Fatal("incomplete snapshot")
		}
		copy := v.Snapshot()
		copy.Selection[0]++
		copy.Generation = "changed"
		if v.Snapshot() != want {
			t.Fatal("mutable snapshot")
		}
		for _, rendered := range []string{fmt.Sprintf("%v %#v", v, v), fmt.Sprintf("%v %#v", want, want)} {
			if strings.Contains(rendered, want.Attempt) || strings.Contains(rendered, want.Origin) {
				t.Fatal("data leaked")
			}
		}
		refuse(nil, b)
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		refuse(canceled, b)
		refuse(ctx, InstalledBinding{})
		original := s.receipt
		s.receipt = nil
		refuse(ctx, b)
		s.receipt = original
		acknowledged := s.acknowledged
		s.acknowledged = nil
		refuse(ctx, b)
		s.acknowledged = acknowledged
		generation := s.runtimeGeneration
		s.runtimeGeneration = ""
		refuse(ctx, b)
		s.runtimeGeneration = generation
		changed := *s.receipt
		changed.OperationID = "foreign"
		s.receipt = &changed
		refuse(ctx, b)
		s.receipt = original
		if _, err := s.AcknowledgedPublication(ctx, b); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if v, e := retained.AcknowledgedPublication(context.Background(), b); e == nil || v != (AcknowledgedPublication{}) {
		t.Fatal("retired data")
	}
	if f.mutationCalls() != 1 {
		t.Fatal("getter mutated CLI")
	}
}
