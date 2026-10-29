package conversationlife

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func heldReceipt(receipt <-chan struct{}) bool {
	timer := time.NewTimer(20 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-receipt:
		return false
	case <-timer.C:
		return true
	}
}

func TestA1ScopedAdmission(t *testing.T) {
	o, foreign := New(), New()
	f := a1NewFixture(t, o, foreign)
	rootHold, releaseRoot := f.hold()
	childHold, releaseChild := f.hold()
	rootEntered, childEntered := a1NewNotice(), a1NewNotice()
	var parentCapture, childCapture atomic.Pointer[Call]
	var rootErr, childErr error
	rootReturned := f.launch(func() {
		rootErr = o.Do(context.Background(), func(c *Call) error {
			// Preserve the first identity even if a broken caller repeats entry.
			parentCapture.CompareAndSwap(nil, c)
			rootEntered.notify()
			<-rootHold
			return nil
		})
	})
	f.require(t, rootEntered.ch, "root entry")
	parent := parentCapture.Load()
	if parent == nil {
		f.finish()
		t.Fatal("root did not publish scope")
	}
	var receipt <-chan struct{}
	fenced := f.launch(func() { receipt = o.Begin() })
	f.require(t, fenced, "root fence return")
	if receipt == nil {
		f.finish()
		t.Fatal("missing actual operation receipt")
	}
	f.require(t, parent.Context().Done(), "root owner cancellation")

	// Every probe is registered before launch, including refusal-only probes:
	// a retained admission gate must never block the sole holder releaser.
	probe := func(label string, fn func() error) error {
		var result error
		returned := f.launch(func() { result = fn() })
		f.require(t, returned, label)
		return result
	}
	var effects, nestedEffects atomic.Int32
	callback := func(*Call) error { effects.Add(1); return nil }
	lateErr := probe("late root return", func() error { return o.Do(context.Background(), callback) })
	nilErr := probe("nil scope return", func() error { return o.DoIn(nil, callback) })
	foreignErr := probe("live foreign scope return", func() error { return foreign.DoIn(parent, callback) })

	// This independently held child enters only AFTER the completed root fence.
	childReturned := f.launch(func() {
		childErr = o.DoIn(parent, func(c *Call) error {
			childCapture.CompareAndSwap(nil, c)
			childEntered.notify()
			<-childHold
			return nil
		})
	})
	f.require(t, childEntered.ch, "child entry after fence")
	child := childCapture.Load()
	if child == nil {
		f.finish()
		t.Fatal("child did not publish scope")
	}
	f.require(t, child.Context().Done(), "child owner cancellation")
	bothCounted := heldReceipt(receipt)
	rootHeld := heldReceipt(rootReturned)
	childHeld := heldReceipt(childReturned)

	releaseRoot()
	f.require(t, rootReturned, "parent return and revocation")
	staleParentErr := probe("stale parent return", func() error { return o.DoIn(parent, callback) })
	nestedErr := probe("live child nesting return", func() error {
		return o.DoIn(child, func(*Call) error { nestedEffects.Add(1); return nil })
	})
	// Actual operation-zero and producer-return channels, not Join-wait proof.
	// Shared lifetime fixtures independently observe the real Join boundaries.
	childCounted := heldReceipt(receipt)
	childStillHeld := heldReceipt(childReturned)
	releaseChild()
	f.require(t, childReturned, "child return and revocation")
	f.require(t, receipt, "actual operation zero")
	joined := f.launch(o.Join)
	f.require(t, joined, "owner Join return after work")
	staleChildErr := probe("stale child after Join return", func() error { return o.DoIn(child, callback) })
	// No launches follow finish: it requests both fences without awaiting them,
	// releases BOTH holders, joins the fixed caller list, then joins both owners.
	f.finish()

	if !errors.Is(lateErr, ErrRefused) || !errors.Is(nilErr, ErrRefused) || !errors.Is(foreignErr, ErrRefused) {
		t.Error("late/nil/foreign refusal", lateErr, nilErr, foreignErr)
	}
	if !errors.Is(staleParentErr, ErrRefused) || !errors.Is(staleChildErr, ErrRefused) {
		t.Error("retained scope accepted", staleParentErr, staleChildErr)
	}
	if nestedErr != nil || nestedEffects.Load() != 1 {
		t.Error("live child nesting", nestedErr, nestedEffects.Load())
	}
	if !bothCounted || !rootHeld || !childHeld || !childCounted || !childStillHeld {
		t.Error("actual operation count/return completed while held", bothCounted, rootHeld, childHeld, childCounted, childStillHeld)
	}
	if rootEntered.count.Load() != 1 || childEntered.count.Load() != 1 || parent.scope == child.scope {
		t.Error("root/child entry count or independent scope identity", rootEntered.count.Load(), childEntered.count.Load())
	}
	if rootErr != nil || childErr != nil || effects.Load() != 0 {
		t.Error("admission/operation result", rootErr, childErr, effects.Load())
	}
	if o.Witnesses() != nil || foreign.Witnesses() != nil {
		t.Error("admission/cancellation poisoned cleanup witnesses")
	}
}

func TestA1Cancellation(t *testing.T) {
	for _, name := range []string{"caller", "owner"} {
		t.Run(name, func(t *testing.T) {
			o := New()
			f := a1NewFixture(t, o)
			ctx, cancel := context.WithCancel(context.Background())
			f.releases = append(f.releases, cancel)
			release, _ := f.hold()
			entered, boundary := a1NewNotice(), a1NewNotice()
			o.joinOperationWait = boundary.notify
			var captureOnce sync.Once
			var captured context.Context
			var result error
			f.launch(func() {
				result = o.Do(ctx, func(c *Call) error {
					captureOnce.Do(func() { captured = c.Context() })
					// Every Do return publishes the first capture before its notice,
					// even if a repeat entry sends the observed notice first.
					entered.notify()
					<-release
					return c.Context().Err()
				})
			})
			f.require(t, entered.ch, "operation entry")
			merged := captured
			if name == "caller" {
				f.launch(cancel)
			} else {
				f.launch(func() { o.Begin() })
			}
			// Record BEFORE cleanup can deliver owner cancellation.
			cancelled := a1Await(merged.Done())
			if !cancelled {
				f.finish()
				t.Fatal("selected cancellation did not arrive before cleanup")
			}
			joined := f.launch(o.Join)
			f.require(t, boundary.ch, "actual Join operation wait")
			held := heldReceipt(joined)
			f.finish()
			if !held {
				t.Error("actual Join returned while callback held")
			}
			if entered.count.Load() != 1 || !errors.Is(result, context.Canceled) || o.Witnesses() != nil {
				t.Error("cancellation/entry/cleanup result", result, entered.count.Load())
			}
		})
	}
}

func TestA1BridgeStopOrJoin(t *testing.T) {
	t.Run("stop", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		f := a1NewFixture(t)
		f.releases = append(f.releases, cancel)
		var calls atomic.Int32
		stop, receipt := bridge(ctx, func() { calls.Add(1) })
		f.bridges = append(f.bridges, func() { stop(); <-receipt })
		// The successful stop path must be observed before cleanup cancels.
		returned := f.launch(stop)
		stopped := a1Await(returned) && a1Await(receipt)
		f.finish()
		stop()
		if !stopped {
			t.Error("stop caller/receipt did not return before cleanup")
		}
		if calls.Load() != 0 {
			t.Error("stopped bridge invoked callback")
		}
	})
	t.Run("started", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		f := a1NewFixture(t)
		f.releases = append(f.releases, cancel)
		release, _ := f.hold()
		entered, boundary := a1NewNotice(), a1NewNotice()
		stop, receipt := observedBridge(ctx, func() { entered.notify(); <-release }, boundary.notify, nil)
		f.bridges = append(f.bridges, func() { stop(); <-receipt })
		f.launch(cancel)
		f.require(t, entered.ch, "started bridge callback")
		returned := f.launch(stop)
		f.require(t, boundary.ch, "actual started bridge stop wait")
		held := heldReceipt(returned)
		f.finish()
		stop()
		<-receipt
		if !held || entered.count.Load() != 1 || boundary.count.Load() != 1 {
			t.Error("actual bridge stop wait/entry proof", held, entered.count.Load(), boundary.count.Load())
		}
	})
}

func TestA1FirstCloseLedger(t *testing.T) {
	o := New()
	t.Cleanup(o.Join)
	shared := errors.New("equal independent witness")
	var closes atomic.Int32
	slot, err := o.Own(func() error { closes.Add(1); return shared })
	if err != nil {
		t.Fatal(err)
	}
	var work sync.WaitGroup
	t.Cleanup(work.Wait)
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		work.Add(1)
		go func() { defer work.Done(); results <- slot.Close() }()
	}
	work.Wait()
	close(results)
	for err := range results {
		if err != shared {
			t.Error("cached identity lost")
		}
	}
	if closes.Load() != 1 {
		t.Error("multiple closes")
	}
	for i := 1; i < WitnessLimit+2; i++ {
		s, _ := o.Own(func() error { return shared })
		s.Close()
	}
	ledger := o.Witnesses()
	leaves := ledger.(interface{ Unwrap() []error }).Unwrap()
	if len(leaves) != WitnessLimit+1 {
		t.Error("ledger bound", len(leaves))
	}
	for _, err := range leaves[:WitnessLimit] {
		if err != shared {
			t.Error("independent equal identity lost")
		}
	}
	var overflow *Overflow
	if !errors.As(ledger, &overflow) || overflow.Count != 2 {
		t.Error("overflow missing", overflow)
	}
	o.ledgerMu.Lock()
	o.overflow = ^uint64(0)
	o.ledgerMu.Unlock()
	s, _ := o.Own(func() error { return shared })
	s.Close()
	if !errors.As(o.Witnesses(), &overflow) || overflow.Count != ^uint64(0) {
		t.Error("overflow wrapped")
	}
}

func TestA1JoinBeforeFinalizers(t *testing.T) {
	o := New()
	t.Cleanup(o.Join)
	witness := errors.New("late retained finalizer")
	slot, _ := o.Own(func() error { return witness })
	o.Join()
	if o.Witnesses() != nil {
		t.Error("premature witness")
	}
	slot.Close()
	if !errors.Is(o.Witnesses(), witness) {
		t.Error("join froze final result")
	}
}
