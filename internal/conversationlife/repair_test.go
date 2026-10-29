package conversationlife

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

func TestA1JoinCancellationCompletion(t *testing.T) {
	o := New()
	f := a1NewFixture(t, o)
	hold, release := f.hold()
	entered, boundary := a1NewNotice(), a1NewNotice()
	realCancel := o.cancel
	o.cancel = func() { realCancel(); entered.notify(); <-hold }
	o.joinCancelWait = boundary.notify
	winner := f.launch(func() { o.Begin() })
	f.require(t, entered.ch, "winning Begin cancellation hold")
	joined := f.launch(o.Join)
	f.require(t, boundary.ch, "Join cancellation-completion boundary")
	held := heldReceipt(joined)
	release()
	f.require(t, winner, "winning Begin return")
	f.require(t, joined, "actual Join return")
	entries, boundaries := entered.count.Load(), boundary.count.Load()
	f.finish()
	if !held {
		t.Error("Join returned before winning cancellation completed")
	}
	if entries != 1 || boundaries != 1 {
		t.Error("unexpected cancellation/boundary count", entries, boundaries)
	}
}

func TestA1JoinGateBoundary(t *testing.T) {
	o := New()
	f := a1NewFixture(t, o)
	// No live operations and no competing mutex users at either probe. Both
	// completion receipts are ready before the observed Join is launched.
	var probes, acquired atomic.Int32
	probe := func() {
		probes.Add(1)
		if o.mu.TryLock() {
			acquired.Add(1)
			o.mu.Unlock()
		}
	}
	o.joinCancelWait, o.joinOperationWait = probe, probe
	fenced := f.launch(func() { o.Begin() })
	f.require(t, fenced, "completed fence")
	joined := f.launch(o.Join)
	f.require(t, joined, "uncontended Join return")
	// Snapshot before cleanup's additional Join. This does NOT exercise a live
	// Join holding mu: retirement needs mu, so that counterfactual is excluded.
	count, locks := probes.Load(), acquired.Load()
	f.finish()
	if count != 2 || locks != 2 {
		t.Error("Join boundary retained gate", count, locks)
	}
}

// Background's nil Done means AfterFunc cannot launch a callback. Only the
// explicit synchronous Err method is abnormal; no custom AfterFunc is supplied.
type a1AbnormalContext struct {
	context.Context
	goexit  bool
	reached *atomic.Int32
	marker  any
}

func (c a1AbnormalContext) Err() error {
	c.reached.Add(1)
	if c.goexit {
		runtime.Goexit()
	}
	panic(c.marker)
}

type a1AbnormalResult struct {
	recovered any
	returned  bool
}

func a1AbnormalProducer(marker any, fn func()) (<-chan struct{}, *a1AbnormalResult) {
	done := make(chan struct{})
	result := &a1AbnormalResult{}
	go func() {
		defer close(done)
		defer func() {
			result.recovered = recover()
			if result.recovered != nil && result.recovered != marker {
				panic(result.recovered)
			}
		}()
		fn()
		result.returned = true
	}()
	return done, result
}
func a1CheckAbnormal(t *testing.T, name string, result *a1AbnormalResult, marker any, reached int32) {
	t.Helper()
	if reached != 1 || result.returned {
		t.Error("setup did not exit at expected Err", reached, result.returned)
	}
	if name == "panic" && result.recovered != marker {
		t.Error("unexpected panic identity", result.recovered)
	}
	if name == "goexit" && result.recovered != nil {
		t.Error("Goexit unexpectedly panicked", result.recovered)
	}
}

func TestA1ContextSetupAbnormalExit(t *testing.T) {
	for _, name := range []string{"panic", "goexit"} {
		t.Run(name, func(t *testing.T) {
			o := New()
			var reached, callbacks atomic.Int32
			marker := new(int)
			ctx := a1AbnormalContext{Context: context.Background(), goexit: name == "goexit", reached: &reached, marker: marker}
			var producer <-chan struct{}
			var once sync.Once
			var inspected bool
			var live uint64
			// Specialized cleanup: join real work first. A leaked symbolic count must
			// not cause a known-blocked Join, nor be reset to fabricate completion.
			finish := func() {
				once.Do(func() {
					if producer != nil {
						<-producer
					}
					if o.mu.TryLock() {
						inspected, live = true, o.live
						o.mu.Unlock()
					}
					if inspected && live == 0 {
						o.Begin()
						o.Join()
					}
				})
			}
			t.Cleanup(finish)
			var result *a1AbnormalResult
			producer, result = a1AbnormalProducer(marker, func() {
				_ = o.Do(ctx, func(*Call) error { callbacks.Add(1); return nil })
			})
			observed := a1Await(producer)
			finish() // Unconditional real-producer join even when observation timed out.
			if !observed {
				t.Error("missing bounded producer completion")
			}
			if !inspected || live != 0 {
				t.Error("pin unwind incomplete", inspected, live)
			}
			if callbacks.Load() != 0 {
				t.Error("operation callback reached")
			}
			a1CheckAbnormal(t, name, result, marker, reached.Load())
		})
	}
}

func TestA1BridgeSetupRollback(t *testing.T) {
	for _, name := range []string{"panic", "goexit"} {
		t.Run(name, func(t *testing.T) {
			var reached, callbacks atomic.Int32
			marker := new(int)
			ctx := a1AbnormalContext{Context: context.Background(), goexit: name == "goexit", reached: &reached, marker: marker}
			var producer <-chan struct{}
			var fallback func()
			var receipt <-chan struct{}
			var registrations int
			var once sync.Once
			var rolledBack bool
			finish := func() {
				once.Do(func() {
					if producer != nil {
						<-producer
					}
					// Observe BEFORE fallback: fallback must not conceal missing rollback.
					if receipt != nil {
						select {
						case <-receipt:
							rolledBack = true
						default:
						}
					}
					if fallback != nil {
						fallback()
					}
				})
			}
			t.Cleanup(finish)
			var result *a1AbnormalResult
			producer, result = a1AbnormalProducer(marker, func() {
				_, _ = observedBridge(ctx, func() { callbacks.Add(1) }, nil, func(stop func(), done <-chan struct{}) {
					registrations++
					fallback, receipt = stop, done
				})
			})
			observed := a1Await(producer)
			finish()
			if !observed {
				t.Error("missing bounded producer completion")
			}
			if registrations != 1 || fallback == nil || !rolledBack {
				t.Error("registration rollback missing before fallback", registrations, rolledBack)
			}
			if callbacks.Load() != 0 {
				t.Error("nil-Done context launched callback")
			}
			a1CheckAbnormal(t, name, result, marker, reached.Load())
		})
	}
}
