package localcontrol

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
)

// Done is first observed after Dispatch has admitted the call and installed
// request cancellation. This synchronizes queue tests without sleeps/polling.
type installedObservedContext struct {
	context.Context
	observed chan struct{}
	once     sync.Once
}

func (c *installedObservedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.observed) })
	return c.Context.Done()
}

func TestInstalledControlConcurrentMutation(t *testing.T) {
	for _, op := range []string{"publish", "arm"} {
		t.Run(op, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			count := 0
			callback := func(context.Context) error {
				count++
				if count == 1 {
					close(entered)
					<-release
				}
				return nil
			}
			c, r, _ := installedFixture(t, func(b *InstalledBackend) {
				b.Publish = callback
				b.Arm = func(ctx context.Context) (string, error) { return "synthetic-secret", callback(ctx) }
			})
			if op == "publish" {
				r.Consent = installedConsent(t, c, r)
			} else {
				r.OperationID = "one"
			}
			r.Op = op
			data, _ := json.Marshal(r)
			var wg sync.WaitGroup
			t.Cleanup(func() { once.Do(func() { close(release) }); wg.Wait() })
			replies := make(chan []byte, 2)
			wg.Add(1)
			go func() { defer wg.Done(); replies <- c.Dispatch(context.Background(), data) }()
			<-entered
			wg.Add(1)
			go func() { defer wg.Done(); replies <- c.Dispatch(context.Background(), data) }()
			once.Do(func() { close(release) })
			wg.Wait()
			codes := map[string]int{}
			for i := 0; i < 2; i++ {
				var result InstalledReply
				_ = json.Unmarshal(<-replies, &result)
				codes[result.Code]++
			}
			if count != 1 || codes["ok"] != 1 || codes["consumed"] != 1 {
				t.Fatal("duplicate mutation", count, codes)
			}
		})
	}
}

func TestInstalledControlQueuedCancellationPreservesService(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	c, r, _ := installedFixture(t, func(b *InstalledBackend) {
		b.Publish = func(context.Context) error { close(entered); <-release; return nil }
	})
	r.Consent = installedConsent(t, c, r)
	r.Op = "publish"
	data, _ := json.Marshal(r)
	var wg sync.WaitGroup
	t.Cleanup(func() { once.Do(func() { close(release) }); wg.Wait() })
	wg.Add(1)
	go func() { defer wg.Done(); _ = c.Dispatch(context.Background(), data) }()
	<-entered
	// Occupied gate plus an uncancelled context ensures this call must queue.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan []byte, 1)
	observed := &installedObservedContext{Context: ctx, observed: make(chan struct{})}
	wg.Add(1)
	go func() { defer wg.Done(); done <- c.Dispatch(observed, data) }()
	<-observed.observed
	cancel()
	var result InstalledReply
	_ = json.Unmarshal(<-done, &result)
	if result.Code != "cancelled" {
		t.Fatal(result)
	}
	select {
	case <-c.state.ctx.Done():
		t.Fatal("request retired service")
	default:
	}
	once.Do(func() { close(release) })
	wg.Wait()
	r.Op = "status"
	r.Consent = nil
	if got := installedCall(t, c, context.Background(), r); got.Code != "ok" || got.Publication != "acknowledged" {
		t.Fatal(got)
	}
}

func TestInstalledControlCancelledPublicationNoReplay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	count := 0
	c, r, _ := installedFixture(t, func(b *InstalledBackend) { b.Publish = func(context.Context) error { count++; cancel(); return nil } })
	r.Consent = installedConsent(t, c, r)
	r.Op = "publish"
	got := installedCall(t, c, ctx, r)
	if got.Code != "cancelled" || got.Publication != "dispatched_unknown" {
		t.Fatal(got)
	}
	if installedCall(t, c, context.Background(), r).Code != "consumed" || count != 1 {
		t.Fatal("cancelled publication replay")
	}
}

func TestInstalledControlReprintMissingAndCancellation(t *testing.T) {
	for _, mode := range []string{"missing", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c, r, calls := installedFixture(t, func(b *InstalledBackend) {
				b.Reprint = func(context.Context) (string, error) {
					if mode == "cancelled" {
						cancel()
						return "synthetic-secret", nil
					}
					return "", nil
				}
			})
			r.Op = "reprint"
			got := installedCall(t, c, ctx, r)
			if got.Code == "ok" || got.Invitation != "" || calls[3] != 0 || calls[2] != 1 {
				t.Fatal(got)
			}
		})
	}
}
