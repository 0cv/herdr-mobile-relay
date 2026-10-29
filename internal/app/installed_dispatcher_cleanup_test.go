package app

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type appDrainWitness struct{ label string }

func (e *appDrainWitness) Error() string { return e.label }

type appLogicalDispatcher struct {
	ctx      context.Context
	begun    chan struct{}
	entered  chan struct{}
	release  chan struct{}
	beginErr error
	drainErr error
	calls    atomic.Int32
	canceled atomic.Bool
}

func (d *appLogicalDispatcher) BeginInstalledDrain() error {
	d.canceled.Store(d.ctx.Err() != nil)
	close(d.begun)
	return d.beginErr
}
func (d *appLogicalDispatcher) DrainInstalled() error {
	d.calls.Add(1)
	close(d.entered)
	<-d.release
	return d.drainErr
}

func appDrainObserve(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("logical drain observation deadline")
	}
}

func TestInstalledDispatcherCleanupRunFlows(t *testing.T) {
	for _, normal := range []bool{false, true} {
		name := "startup"
		if normal {
			name = "normal_tail"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			beginErr := &appDrainWitness{"begin"}
			drainErr := &appDrainWitness{"drain"}
			runErr := &appDrainWitness{"run"}
			d := &appLogicalDispatcher{ctx: ctx, begun: make(chan struct{}), entered: make(chan struct{}), release: make(chan struct{}), beginErr: beginErr, drainErr: errors.Join(context.Canceled, drainErr)}
			owner := newInstalledDispatcherCleanup(cancel, d)
			done := make(chan struct{})
			var result error
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(d.release) }) }
			defer func() { cancel(); release(); <-done }()
			go func() {
				defer close(done)
				result = func() (err error) {
					defer func() { err = errors.Join(err, owner.drain()) }()
					if normal {
						owner.begin()
						_ = owner.drain()
					}
					return runErr
				}()
			}()
			appDrainObserve(t, d.begun)
			appDrainObserve(t, d.entered)
			if !d.canceled.Load() {
				t.Fatal("Begin preceded root cancellation")
			}
			select {
			case <-done:
				t.Fatal("return escaped held drain")
			default:
			}
			release()
			<-done
			for _, want := range []error{runErr, beginErr, drainErr, context.Canceled} {
				if !errors.Is(result, want) {
					t.Fatalf("lost sibling %v: %v", want, result)
				}
			}
			var typed *appDrainWitness
			if !errors.As(result, &typed) {
				t.Fatal("typed witness laundered")
			}
			if d.calls.Load() != 1 {
				t.Fatal("physical drain repeated")
			}
			// Count leaf identities, not just errors.Is: a normal+defer double
			// collection would otherwise be invisible.
			var count func(error, error) int
			count = func(err, target error) int {
				if err == target {
					return 1
				}
				if joined, ok := err.(interface{ Unwrap() []error }); ok {
					n := 0
					for _, child := range joined.Unwrap() {
						n += count(child, target)
					}
					return n
				}
				return 0
			}
			for _, want := range []error{runErr, beginErr, drainErr, context.Canceled} {
				if count(result, want) != 1 {
					t.Fatalf("sibling collected more than once: %v", want)
				}
			}
		})
	}
}

func TestInstalledDispatcherCleanupConcurrentCachedResult(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	witness := &appDrainWitness{"joined"}
	d := &appLogicalDispatcher{ctx: ctx, begun: make(chan struct{}), entered: make(chan struct{}), release: make(chan struct{}), drainErr: witness}
	owner := newInstalledDispatcherCleanup(cancel, d)
	const callers = 8
	results := make([]error, callers)
	var wg sync.WaitGroup
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(d.release) }) }
	defer func() { cancel(); release(); wg.Wait() }()
	for i := range results {
		wg.Add(1)
		go func() { defer wg.Done(); results[i] = owner.drain() }()
	}
	appDrainObserve(t, d.entered)
	if !d.canceled.Load() {
		t.Fatal("root not canceled before drain")
	}
	release()
	wg.Wait()
	for _, result := range results {
		if result != results[0] || !errors.Is(result, witness) {
			t.Fatal("cached result changed")
		}
	}
	if owner.drain() != results[0] || d.calls.Load() != 1 {
		t.Fatal("drain not cached")
	}
}
