package localcontrol

import (
	"context"
	"errors"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

func TestInstalledAcknowledgementUncertainPublication(t *testing.T) {
	calls := 0
	c, r, _ := installedFixture(t, func(b *InstalledBackend) {
		b.Publish = func(context.Context) error { return errors.New("uncertain publication") }
		b.PublicationSelection = [32]byte{1}
		b.AcknowledgedPublication = func(context.Context) (tailscalecli.AcknowledgedPublication, error) {
			calls++
			return tailscalecli.AcknowledgedPublication{}, nil
		}
	})
	r.Consent = installedConsent(t, c, r)
	r.Op = "publish"
	if installedCall(t, c, context.Background(), r).Publication != "dispatched_unknown" {
		t.Fatal("uncertainty lost")
	}
	for _, ctx := range []context.Context{nil, context.Background()} {
		if v, err := c.AcknowledgedPublication(ctx); err == nil || v != (tailscalecli.AcknowledgedPublication{}) {
			t.Fatal("uncertain data accepted")
		}
	}
	if calls != 0 {
		t.Fatal("uncertain callback entered")
	}
}
