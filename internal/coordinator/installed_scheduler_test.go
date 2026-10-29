package coordinator

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func installedAwait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("channel barrier not reached")
	}
}
func TestInstalledSchedulerCanceledCloseHeldEffect(t *testing.T) {
	s := newInstalledScheduler(1, nil)
	entered := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	unlock := func() { once.Do(func() { close(release) }) }
	var callers sync.WaitGroup
	defer func() { _ = s.beginInstalledDrain(); unlock(); callers.Wait(); _ = s.drainInstalled() }()
	callers.Add(1)
	go func() {
		defer callers.Done()
		_, _ = s.Execute(context.Background(), ScheduleOptions{RelayLevel: true, Deadline: time.Now().Add(time.Hour)}, EffectFunc(func(ctx context.Context, _ WorkerToken) EffectResult {
			close(entered)
			<-ctx.Done()
			close(canceled)
			<-release
			return EffectResult{}
		}))
	}()
	installedAwait(t, entered)
	queued := make(chan struct{})
	var dispatched atomic.Bool
	callers.Add(1)
	go func() {
		defer callers.Done()
		_, _ = s.ExecuteAdmitted(context.Background(), ScheduleOptions{RelayLevel: true, Deadline: time.Now().Add(time.Hour)}, EffectFunc(func(context.Context, WorkerToken) EffectResult { dispatched.Store(true); return EffectResult{} }), func() { close(queued) })
	}()
	installedAwait(t, queued)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	installedAwait(t, canceled)
	if _, err := s.Execute(context.Background(), ScheduleOptions{}, nil); !errors.Is(err, ErrClosed) {
		t.Fatal("late admission", err)
	}
	select {
	case <-s.installed.done:
		t.Fatal("drained held effect")
	default:
	}
	unlock()
	if err := s.drainInstalled(); err != nil {
		t.Fatal(err)
	}
	callers.Wait()
	if dispatched.Load() {
		t.Fatal("queued effect dispatched")
	}
}
func TestInstalledSchedulerCompletionIsNotEffectJoin(t *testing.T) {
	held := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	unlock := func() { once.Do(func() { close(release) }) }
	owned := &installedScheduler{stop: make(chan struct{}), done: make(chan struct{}), afterCompletion: func() { close(held); <-release }}
	s := newScheduler(1, nil, owned)
	var callers sync.WaitGroup
	defer func() { _ = s.beginInstalledDrain(); unlock(); callers.Wait(); _ = s.drainInstalled() }()
	returned := make(chan struct{})
	callers.Add(1)
	go func() {
		defer callers.Done()
		defer close(returned)
		_, _ = s.Execute(context.Background(), ScheduleOptions{RelayLevel: true, Deadline: time.Now().Add(time.Hour)}, EffectFunc(func(context.Context, WorkerToken) EffectResult { return EffectResult{} }))
	}()
	installedAwait(t, held)
	installedAwait(t, returned)
	_ = s.beginInstalledDrain()
	installedAwait(t, s.done) // completion consumed and owner exited, effect still held
	select {
	case <-owned.done:
		t.Fatal("completion mistaken for join")
	default:
	}
	var drains sync.WaitGroup
	results := make(chan error, 12)
	defer drains.Wait()
	// This defer must release before joining concurrent drain waiters on failure.
	defer unlock()
	for i := 0; i < 6; i++ {
		drains.Add(2)
		go func() { defer drains.Done(); results <- s.drainInstalled() }()
		go func() { defer drains.Done(); results <- s.Close(context.Background()) }()
	}
	unlock()
	drains.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := s.drainInstalled(); err != nil {
		t.Fatal(err)
	}
}
func TestInstalledSchedulerRefusesUnownedAndNilContext(t *testing.T) {
	var zero Scheduler
	if !errors.Is(zero.drainInstalled(), errNotInstalledScheduler) {
		t.Fatal("zero accepted")
	}
	var absent *Scheduler
	if !errors.Is(absent.drainInstalled(), errNotInstalledScheduler) {
		t.Fatal("nil accepted")
	}
	ordinary := NewScheduler(1, nil)
	defer ordinary.Close(context.Background())
	if !errors.Is(ordinary.drainInstalled(), errNotInstalledScheduler) {
		t.Fatal("ordinary accepted")
	}
	s := newInstalledScheduler(1, nil)
	defer s.drainInstalled()
	if !errors.Is(s.Close(nil), errInstalledSchedulerContext) {
		t.Fatal("nil context accepted")
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatal("completed close", err)
	}
}
