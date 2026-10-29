package localcontrol

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

func TestInstalledAcknowledgementQueueDrain(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	c, r, _ := installedFixture(t, func(b *InstalledBackend) {
		b.PublicationSelection = [32]byte{1}
		b.AcknowledgedPublication = func(ctx context.Context) (tailscalecli.AcknowledgedPublication, error) {
			once.Do(func() { close(entered) })
			<-release
			return tailscalecli.AcknowledgedPublication{}, ctx.Err()
		}
	})
	r.Consent = installedConsent(t, c, r)
	r.Op = "publish"
	if installedCall(t, c, context.Background(), r).Publication != "acknowledged" {
		t.Fatal("publish refused")
	}
	ctx, cancel := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	retired := make(chan struct{})
	retiring := false
	defer func() {
		cancel()
		close(release)
		workers.Wait()
		if retiring {
			<-retired
		}
	}()
	workers.Add(1)
	go func() { defer workers.Done(); _, _ = c.AcknowledgedPublication(ctx) }()
	select {
	case <-entered:
	case <-time.After(time.Second * 5):
		t.Fatal("holder not entered")
	}
	queued, stopQueued := context.WithCancel(ctx)
	defer stopQueued()
	queuedDone := make(chan struct{})
	workers.Add(1)
	go func() { defer workers.Done(); defer close(queuedDone); _, _ = c.AcknowledgedPublication(queued) }()
	waitSlots := func(want int) {
		t.Helper()
		deadline := time.NewTimer(time.Second * 5)
		defer deadline.Stop()
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for len(c.state.slots) != want {
			select {
			case <-deadline.C:
				t.Fatal("queue entry not observed")
			case <-tick.C:
			}
		}
	}
	waitSlots(2)
	stopQueued()
	select {
	case <-queuedDone:
	case <-time.After(time.Second * 5):
		t.Fatal("queued cancellation waited for holder")
	}
	waitSlots(1)
	for i := 0; i < 15; i++ {
		workers.Add(1)
		go func() { defer workers.Done(); _, _ = c.AcknowledgedPublication(ctx) }()
	}
	waitSlots(16)
	if v, err := c.AcknowledgedPublication(ctx); err == nil || v != (tailscalecli.AcknowledgedPublication{}) {
		t.Fatal("excess admission")
	}
	retiring = true
	go func() { c.Retire(); close(retired) }()
	select {
	case <-c.state.ctx.Done():
	case <-time.After(time.Second * 5):
		t.Fatal("retirement did not cancel")
	}
	select {
	case <-retired:
		t.Fatal("retirement escaped held callback")
	default:
	}
	// Deferred cancellation, unconditional release and joins precede fixture cleanup.
}
