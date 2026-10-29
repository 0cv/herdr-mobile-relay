package conversationlife

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Noncomparable contexts are valid inputs. Every method is effectful, so a
// refused admission must not even attempt context setup.
type a2ProbeContext struct{ calls []*atomic.Int32 }

func (c a2ProbeContext) Deadline() (time.Time, bool) { c.calls[0].Add(1); return time.Time{}, false }
func (c a2ProbeContext) Done() <-chan struct{}       { c.calls[0].Add(1); return nil }
func (c a2ProbeContext) Err() error                  { c.calls[0].Add(1); return nil }
func (c a2ProbeContext) Value(any) any               { c.calls[0].Add(1); return nil }

func TestA2ScopedContextAdmission(t *testing.T) {
	o, foreign := New(), New()
	f := a1NewFixture(t, o, foreign)
	hold, release := f.hold()
	entered := a1NewNotice()
	var capture atomic.Pointer[Call]
	root := f.launch(func() {
		_ = o.Do(context.Background(), func(c *Call) error {
			capture.CompareAndSwap(nil, c)
			entered.notify()
			<-hold
			return nil
		})
	})
	f.require(t, entered.ch, "parent entry")
	parent := capture.Load()
	var probes, effects atomic.Int32
	ctx := a2ProbeContext{calls: []*atomic.Int32{&probes}}
	callback := func(*Call) error { effects.Add(1); return nil }
	var missing *Owner
	var zero Owner
	for _, tc := range []struct {
		name   string
		owner  *Owner
		parent *Call
		ctx    context.Context
		fn     func(*Call) error
	}{
		{"nil-owner", missing, parent, ctx, callback},
		{"zero-owner", &zero, parent, ctx, callback},
		{"nil-parent", o, nil, ctx, callback},
		{"empty-parent", o, &Call{}, ctx, callback},
		{"foreign-parent", foreign, parent, ctx, callback},
		{"nil-context", o, parent, nil, callback},
		{"nil-callback", o, parent, ctx, nil},
	} {
		var result error
		job := f.launch(func() { result = tc.owner.DoInContext(tc.ctx, tc.parent, tc.fn) })
		f.require(t, job, tc.name)
		if result != ErrRefused {
			t.Error(tc.name, result)
		}
	}
	release()
	f.require(t, root, "parent retirement")
	var stale error
	job := f.launch(func() { stale = o.DoInContext(ctx, parent, callback) })
	f.require(t, job, "stale refusal")
	f.finish()
	if stale != ErrRefused || probes.Load() != 0 || effects.Load() != 0 || entered.count.Load() != 1 {
		t.Error("refusal effects or identity", stale, probes.Load(), effects.Load(), entered.count.Load())
	}
	if o.Witnesses() != nil || foreign.Witnesses() != nil {
		t.Error("refusal poisoned witnesses")
	}
}

func TestA2ScopedContextDeadlinesAndValues(t *testing.T) {
	early := time.Now().Add(time.Hour)
	late := early.Add(time.Hour)
	for _, tc := range []struct {
		name                 string
		caller, parent, want time.Time
		expired              bool
	}{
		{"caller-only", early, time.Time{}, early, false},
		{"parent-only", time.Time{}, early, early, false},
		{"earlier-caller", early, late, early, false},
		{"earlier-parent", late, early, early, false},
		{"equal", early, early, early, false},
		{"neither", time.Time{}, time.Time{}, time.Time{}, false},
		{"expired-caller", time.Unix(1, 0), time.Time{}, time.Unix(1, 0), true},
		{"expired-parent", time.Time{}, time.Unix(1, 0), time.Unix(1, 0), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := New()
			f := a1NewFixture(t, o)
			type key struct{}
			caller := context.WithValue(context.Background(), key{}, "explicit")
			parent := context.WithValue(context.Background(), key{}, "parent")
			if !tc.caller.IsZero() {
				var cancel context.CancelFunc
				caller, cancel = context.WithDeadline(caller, tc.caller)
				f.releases = append(f.releases, cancel)
			}
			if !tc.parent.IsZero() {
				var cancel context.CancelFunc
				parent, cancel = context.WithDeadline(parent, tc.parent)
				f.releases = append(f.releases, cancel)
			}
			marker := errors.New("exact result")
			var result, observed error
			var value any
			var deadline time.Time
			var ok bool
			var roots, children int
			job := f.launch(func() {
				result = o.Do(parent, func(p *Call) error {
					roots++
					return o.DoInContext(caller, p, func(c *Call) error {
						children++
						value = c.Context().Value(key{})
						deadline, ok = c.Context().Deadline()
						observed = c.Context().Err()
						return marker
					})
				})
			})
			f.require(t, job, "deadline operation return")
			f.finish()
			if result != marker || value != "explicit" || roots != 1 || children != 1 {
				t.Error("result/value/invocations", result, value, roots, children)
			}
			if ok != !tc.want.IsZero() || !deadline.Equal(tc.want) {
				t.Error("deadline", deadline, ok)
			}
			if tc.expired && observed != context.DeadlineExceeded {
				t.Error("expired cause", observed)
			}
			if !tc.expired && observed != nil {
				t.Error("unexpected cancellation", observed)
			}
			if o.Witnesses() != nil {
				t.Error("context poisoned witnesses")
			}
		})
	}
}

func TestA2ScopedContextCancellationAndJoin(t *testing.T) {
	for _, name := range []string{"explicit", "parent-caller", "parent-return", "owner", "after-fence"} {
		t.Run(name, func(t *testing.T) {
			o := New()
			f := a1NewFixture(t, o)
			parentCtx, cancelParent := context.WithCancel(context.Background())
			callerCtx, cancelCaller := context.WithCancel(context.Background())
			f.releases = append(f.releases, cancelParent, cancelCaller)
			parentHold, releaseParent := f.hold()
			childHold, releaseChild := f.hold()
			parentEntered, childEntered, boundary := a1NewNotice(), a1NewNotice(), a1NewNotice()
			o.joinOperationWait = boundary.notify
			var parentCapture, childCapture atomic.Pointer[Call]
			var rootResult, childResult error
			root := f.launch(func() {
				rootResult = o.Do(parentCtx, func(p *Call) error {
					parentCapture.CompareAndSwap(nil, p)
					parentEntered.notify()
					<-parentHold
					return nil
				})
			})
			f.require(t, parentEntered.ch, "parent entry")
			parent := parentCapture.Load()
			if name == "after-fence" {
				fence := f.launch(func() { o.Begin() })
				f.require(t, fence, "fence before child")
			}
			childJob := f.launch(func() {
				childResult = o.DoInContext(callerCtx, parent, func(c *Call) error {
					childCapture.CompareAndSwap(nil, c)
					childEntered.notify()
					<-childHold
					return c.Context().Err()
				})
			})
			f.require(t, childEntered.ch, "child entry")
			child := childCapture.Load()
			switch name {
			case "explicit":
				f.launch(cancelCaller)
			case "parent-caller":
				f.launch(cancelParent)
			case "parent-return":
				releaseParent()
				f.require(t, root, "parent return cancellation")
			case "owner":
				f.launch(func() { o.Begin() })
			}
			// This observation precedes Join, finish, and all fallback cancellation.
			if !a1Await(child.Context().Done()) {
				f.finish()
				t.Fatal("selected cancellation missing before fallback")
			}
			releaseParent()
			f.require(t, root, "parent retirement")
			var staleParent error
			var effects atomic.Int32
			probe := f.launch(func() {
				staleParent = o.DoInContext(context.Background(), parent, func(*Call) error { effects.Add(1); return nil })
			})
			f.require(t, probe, "stale parent refusal")
			joined := f.launch(o.Join)
			f.require(t, boundary.ch, "actual returning Join operation boundary")
			held := heldReceipt(joined)
			counted := heldReceipt(o.done)
			releaseChild()
			f.require(t, childJob, "child return")
			f.require(t, joined, "actual Join completion")
			boundaries := boundary.count.Load()
			var staleChild error
			probe = f.launch(func() {
				staleChild = o.DoInContext(context.Background(), child, func(*Call) error { effects.Add(1); return nil })
			})
			f.require(t, probe, "stale child refusal")
			f.finish()
			if !held || !counted || boundaries != 1 {
				t.Error("Join/count boundary", held, counted, boundaries)
			}
			if parent == child || parent.scope == child.scope || parentEntered.count.Load() != 1 || childEntered.count.Load() != 1 {
				t.Error("scope identity/invocations")
			}
			if rootResult != nil || childResult != context.Canceled || staleParent != ErrRefused || staleChild != ErrRefused || effects.Load() != 0 {
				t.Error("operation results", rootResult, childResult, staleParent, staleChild, effects.Load())
			}
			if o.Witnesses() != nil {
				t.Error("cancellation poisoned witnesses")
			}
		})
	}
}

func TestA2ScopedContextAbnormalExit(t *testing.T) {
	for _, stage := range []string{"setup", "callback"} {
		for _, mode := range []string{"panic", "goexit"} {
			t.Run(stage+"-"+mode, func(t *testing.T) {
				o := New()
				marker := new(int)
				var reached, callbacks atomic.Int32
				var producer <-chan struct{}
				var once sync.Once
				var inspected bool
				var live uint64
				// No holders or external cancellation jobs. The real producer owns
				// both calls; nil Done setup cannot start an abnormal bridge callback.
				// Join it before inspecting counts, never fabricate a zero receipt.
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
					_ = o.Do(context.Background(), func(p *Call) error {
						var ctx context.Context = context.Background()
						if stage == "setup" {
							ctx = a1AbnormalContext{Context: context.Background(), goexit: mode == "goexit", reached: &reached, marker: marker}
						}
						return o.DoInContext(ctx, p, func(*Call) error {
							callbacks.Add(1)
							reached.Add(1)
							if mode == "goexit" {
								runtime.Goexit()
							}
							panic(marker)
						})
					})
				})
				observed := a1Await(producer)
				finish()
				if !observed || !inspected || live != 0 {
					t.Error("abnormal unwind", observed, inspected, live)
				}
				wantCallbacks := int32(0)
				if stage == "callback" {
					wantCallbacks = 1
				}
				if callbacks.Load() != wantCallbacks {
					t.Error("callback count", callbacks.Load())
				}
				a1CheckAbnormal(t, mode, result, marker, reached.Load())
				if o.Witnesses() != nil {
					t.Error("abnormal exit poisoned witnesses")
				}
			})
		}
	}
}
