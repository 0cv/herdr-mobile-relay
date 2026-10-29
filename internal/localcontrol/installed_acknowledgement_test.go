package localcontrol

import (
	"context"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

func TestInstalledAcknowledgementGates(t *testing.T) {
	calls := 0
	c, r, _ := installedFixture(t, func(b *InstalledBackend) {
		b.PublicationSelection = [32]byte{1}
		b.AcknowledgedPublication = func(context.Context) (tailscalecli.AcknowledgedPublication, error) {
			calls++
			return tailscalecli.AcknowledgedPublication{}, nil
		}
	})
	if _, err := c.AcknowledgedPublication(context.Background()); err == nil || calls != 0 {
		t.Fatal("unpublished callback reached")
	}
	r.Consent = installedConsent(t, c, r)
	r.Op = "publish"
	if reply := installedCall(t, c, context.Background(), r); reply.Publication != "acknowledged" {
		t.Fatal("publish refused")
	}
	if _, err := c.AcknowledgedPublication(context.Background()); err == nil || calls != 1 {
		t.Fatal("zero evidence accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.AcknowledgedPublication(ctx); err == nil || calls != 1 {
		t.Fatal("canceled callback reached")
	}
	c.Retire()
	if _, err := c.AcknowledgedPublication(context.Background()); err == nil || calls != 1 {
		t.Fatal("retired callback reached")
	}
}

func TestInstalledAcknowledgementUnavailable(t *testing.T) {
	for _, ctx := range []context.Context{nil, context.Background()} {
		value, err := (InstalledControl{}).AcknowledgedPublication(ctx)
		if err == nil || value != (tailscalecli.AcknowledgedPublication{}) {
			t.Fatal("zero control returned data")
		}
	}
}
