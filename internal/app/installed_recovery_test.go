package app

import (
	"context"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
	"testing"
	"time"
)

func TestInstalledRecoveryLifetime(t *testing.T) {
	for _, terminal := range []error{tailscalecli.ErrConflict, tailscalecli.ErrUncertain, tailscalecli.ErrUnsupported} {
		ctx, cancel := context.WithCancel(context.Background())
		ticks := make(chan time.Time, 20)
		start := time.Unix(1, 0)
		for i := 0; i < 20; i++ {
			ticks <- start.Add(time.Duration(i) * 30 * time.Second)
		}
		close(ticks)
		outcomes := []error{tailscalecli.ErrTransientUnavailable, nil, tailscalecli.ErrTransientUnavailable, nil, terminal}
		calls, closed := 0, 0
		installedRecovery(ctx, ticks, func(ctx context.Context) error {
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("unbounded admission")
			}
			if calls >= len(outcomes) {
				t.Fatal("retried conflict")
			}
			err := outcomes[calls]
			calls++
			return err
		}, func() { closed++ })
		cancel()
		if calls != len(outcomes) || closed != 4 {
			t.Fatalf("calls=%d quarantine=%d", calls, closed)
		}
	}
}

func TestInstalledRecoveryInFlightCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time, 1)
	ticks <- time.Now()
	closed := false
	installedRecovery(ctx, ticks, func(checkCtx context.Context) error {
		cancel()
		<-checkCtx.Done()
		return checkCtx.Err()
	}, func() { closed = true })
	if !closed {
		t.Fatal("cancellation left admission open")
	}
}

func TestInstalledRecoveryCancellationAndBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time, 25)
	for i := 0; i < 25; i++ {
		ticks <- time.Unix(1, 0).Add(time.Duration(i) * 5 * time.Second)
	}
	close(ticks)
	calls := 0
	installedRecovery(ctx, ticks, func(context.Context) error { calls++; return tailscalecli.ErrTransientUnavailable }, func() {})
	if calls != 2 {
		t.Fatalf("tight polling: %d", calls)
	}
	cancel()
	installedRecovery(ctx, ticks, func(context.Context) error { t.Fatal("check after cancel"); return nil }, func() {})
}
