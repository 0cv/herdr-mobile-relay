package coordinator

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestInstalledSchedulerReentrantFinalizerBegin(t *testing.T)   { installedFinalizerStop(t, true) }
func TestInstalledSchedulerHeldFinalizerCancellation(t *testing.T) { installedFinalizerStop(t, false) }

func installedFinalizerStop(t *testing.T, reentrant bool) {
	t.Helper()
	s := newInstalledScheduler(2, nil)
	entered, canceled := make(chan struct{}), make(chan struct{})
	held, release := make(chan struct{}), make(chan struct{})
	beginDone := make(chan struct{})
	var calls sync.WaitGroup
	var once sync.Once
	unlock := func() { once.Do(func() { close(release) }) }
	var ran atomic.Bool
	denied := EffectFunc(func(context.Context, WorkerToken) EffectResult { ran.Store(true); return EffectResult{} })
	defer func() {
		// Independent lifetime cancellation and all holder releases precede joins,
		// including a Begin that a regressed finalizer fence could block.
		s.installed.cancel()
		unlock()
		_ = s.beginInstalledDrain()
		calls.Wait()
		_ = s.drainInstalled()
	}()
	calls.Add(1)
	go func() {
		defer calls.Done()
		_, _ = s.Execute(context.Background(), ScheduleOptions{RelayLevel: true, Deadline: time.Now().Add(time.Hour)}, EffectFunc(func(ctx context.Context, _ WorkerToken) EffectResult {
			close(entered)
			<-ctx.Done()
			close(canceled)
			<-release
			return EffectResult{}
		}))
	}()
	installedAwait(t, entered)
	calls.Add(1)
	go func() {
		defer calls.Done()
		_, _ = s.Execute(context.Background(), ScheduleOptions{RelayLevel: true, Deadline: time.Now().Add(time.Hour)}, EffectFunc(func(context.Context, WorkerToken) EffectResult {
			return EffectResult{Continuation: &EffectContinuation{PaneID: "logical", Deadline: time.Now().Add(-time.Second), Runner: denied, Finalize: func(r *CommandResult, _ error) *CommandResult {
				if reentrant {
					calls.Add(1)
					go func() { defer calls.Done(); _ = s.beginInstalledDrain(); close(beginDone) }()
				}
				close(held)
				if reentrant {
					select {
					case <-beginDone:
					case <-release:
					}
				}
				<-release
				return r
			}}}
		}))
	}()
	installedAwait(t, held)
	queued := make(chan struct{})
	calls.Add(1)
	go func() {
		defer calls.Done()
		_, _ = s.ExecuteAdmitted(context.Background(), ScheduleOptions{RelayLevel: true, Deadline: time.Now().Add(time.Hour)}, denied, func() { close(queued) })
	}()
	installedAwait(t, queued)
	if !reentrant {
		calls.Add(1)
		go func() { defer calls.Done(); _ = s.beginInstalledDrain(); close(beginDone) }()
	}
	installedAwait(t, beginDone)
	installedAwait(t, canceled)
	select {
	case <-s.installed.done:
		t.Fatal("drained held finalizer/effect")
	default:
	}
	unlock()
	calls.Wait()
	if err := s.drainInstalled(); err != nil {
		t.Fatal(err)
	}
	if ran.Load() {
		t.Fatal("refused continuation or queued work dispatched")
	}
}

func TestInstalledSchedulerRequestContextRefusal(t *testing.T) {
	s := newInstalledScheduler(1, nil)
	defer s.drainInstalled()
	var ran atomic.Bool
	runner := EffectFunc(func(context.Context, WorkerToken) EffectResult { ran.Store(true); return EffectResult{} })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, c := range []context.Context{nil, ctx} {
		count := 0
		_, err := s.ExecuteAdmitted(c, ScheduleOptions{RelayLevel: true, Deadline: time.Now().Add(time.Hour)}, runner, func() { count++ })
		if err == nil || count != 1 {
			t.Fatal("invalid context admission", err, count)
		}
	}
	if ran.Load() {
		t.Fatal("invalid context dispatched")
	}
}

type installedObservedContext struct {
	context.Context
	checked chan struct{}
	once    sync.Once
}

func (c *installedObservedContext) Err() error {
	err := c.Context.Err()
	c.once.Do(func() { close(c.checked) })
	return err
}

func TestInstalledSchedulerContextCanceledAtGate(t *testing.T) {
	s := newInstalledScheduler(1, nil)
	parent, cancel := context.WithCancel(context.Background())
	ctx := &installedObservedContext{Context: parent, checked: make(chan struct{})}
	s.installed.mu.Lock()
	var once sync.Once
	unlock := func() { once.Do(s.installed.mu.Unlock) }
	var calls sync.WaitGroup
	defer func() { cancel(); unlock(); calls.Wait(); _ = s.drainInstalled() }()
	var ran atomic.Bool
	var acknowledgements atomic.Int32
	result := make(chan error, 1)
	calls.Add(1)
	go func() {
		defer calls.Done()
		_, err := s.ExecuteAdmitted(ctx, ScheduleOptions{RelayLevel: true, Deadline: time.Now().Add(time.Hour)}, EffectFunc(func(context.Context, WorkerToken) EffectResult { ran.Store(true); return EffectResult{} }), func() { acknowledgements.Add(1) })
		result <- err
	}()
	installedAwait(t, ctx.checked) // pre-admission Err read completed while gate held
	cancel()
	unlock()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("context refusal blocked")
	}
	calls.Wait()
	if ran.Load() || acknowledgements.Load() != 1 {
		t.Fatal("context admission")
	}
}
