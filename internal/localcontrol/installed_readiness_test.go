package localcontrol

import (
	"context"
	"errors"
	"testing"
)

// Synthetic read-only readiness; no app, filesystem, HTTP or listener effects.
func TestInstalledControlReadinessPreservesProposal(t *testing.T) {
	ready, checks := false, 0
	c, r, calls := installedFixture(t, func(b *InstalledBackend) {
		b.BeforePublish = func(context.Context) error {
			checks++
			if !ready {
				return errors.New("private readiness diagnostic")
			}
			return nil
		}
	})
	consent := installedConsent(t, c, r)
	r.Op, r.Consent = "publish", consent
	bad := r
	bad.Capability = ""
	if got := installedCall(t, c, context.Background(), bad); got.Code != "unauthorized" {
		t.Fatal(got)
	}
	wrong := *consent
	wrong.Production = false
	bad = r
	bad.Consent = &wrong
	if got := installedCall(t, c, context.Background(), bad); got.Code != "consent_refused" {
		t.Fatal(got)
	}
	if checks != 0 || calls[0] != 0 {
		t.Fatal("work before authentication and consent")
	}
	for i := 0; i < 2; i++ {
		if got := installedCall(t, c, context.Background(), r); got.Code != "readiness_unavailable" || got.Publication != "not_dispatched" {
			t.Fatal(got)
		}
	}
	if calls[0] != 0 {
		t.Fatal("unready publication dispatched")
	}
	proposal := r
	proposal.Op, proposal.Consent = "proposal", nil
	if got := installedCall(t, c, context.Background(), proposal); got.Proposal == nil || *got.Proposal != consent.Proposal {
		t.Fatal("proposal replaced")
	}
	ready = true
	if got := installedCall(t, c, context.Background(), r); got.Publication != "acknowledged" {
		t.Fatal(got)
	}
	if got := installedCall(t, c, context.Background(), r); got.Code != "consumed" {
		t.Fatal(got)
	}
	if checks != 3 || calls[0] != 1 {
		t.Fatal("replay entered preflight or publication")
	}
}

func TestInstalledControlReadinessCancellationUnconsumed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	checks := 0
	c, r, calls := installedFixture(t, func(b *InstalledBackend) {
		b.BeforePublish = func(context.Context) error {
			checks++
			if checks == 1 {
				cancel()
			}
			return nil
		}
	})
	r.Consent = installedConsent(t, c, r)
	r.Op = "publish"
	if got := installedCall(t, c, ctx, r); got.Code != "cancelled" || got.Publication != "not_dispatched" {
		t.Fatal(got)
	}
	if calls[0] != 0 {
		t.Fatal("cancelled preflight dispatched")
	}
	if got := installedCall(t, c, context.Background(), r); got.Publication != "acknowledged" {
		t.Fatal(got)
	}
	if calls[0] != 1 || checks != 2 {
		t.Fatal("same proposal not reusable")
	}
}

func TestInstalledControlReadinessDispatchStillConsumed(t *testing.T) {
	checks := 0
	c, r, _ := installedFixture(t, func(b *InstalledBackend) {
		b.BeforePublish = func(context.Context) error { checks++; return nil }
		b.Publish = func(context.Context) error { return errors.New("private publication uncertainty") }
	})
	r.Consent = installedConsent(t, c, r)
	r.Op = "publish"
	if got := installedCall(t, c, context.Background(), r); got.Code != "outcome_unknown" || got.Publication != "dispatched_unknown" {
		t.Fatal(got)
	}
	if got := installedCall(t, c, context.Background(), r); got.Code != "consumed" {
		t.Fatal(got)
	}
	if checks != 1 {
		t.Fatal("consumed preflight repeated")
	}
}

func TestInstalledControlResumeRejectsReadinessCapability(t *testing.T) {
	c, _, _ := installedFixture(t, nil)
	b := c.state.backend
	b.Publish = nil
	b.BeforePublish = func(context.Context) error { t.Fatal("resume readiness invoked"); return nil }
	selection := c.state.proposal.InstalledSelection
	selection.Attempt = ""
	h, credentials, err := NewInstalledResumeControl("installation", selection, b)
	defer h.Retire()
	if err == nil || h.state != nil || credentials != (InstalledCredentials{}) {
		t.Fatal("mixed resume readiness capability accepted")
	}
}
