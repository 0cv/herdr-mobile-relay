package coordinator

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestInstalledDispatcherActualQueuedAndEnteredCancellation(t *testing.T) {
	d := dispatcherFixture(t, nil)
	release := make(chan struct{})
	var once sync.Once
	unlock := func() { once.Do(func() { close(release) }) }
	var callers sync.WaitGroup
	var observers sync.WaitGroup
	defer func() { dispatcherStopForCleanup(d); unlock(); callers.Wait(); observers.Wait(); d.DrainInstalled() }()
	// Occupy every actual permit with a logical effect, observing each entry
	// rather than inferring acceptance from queue lengths or timing.
	entered := make(chan struct{}, defaultHerdrCapacity)
	canceled := make(chan struct{}, defaultHerdrCapacity)
	for i := 0; i < defaultHerdrCapacity; i++ {
		callers.Add(1)
		go func() {
			defer callers.Done()
			d.HandleTopologyAdmitted(context.Background(), nil, func(ctx context.Context) *CommandResult {
				result, _ := d.scheduler.Execute(ctx, ScheduleOptions{RelayLevel: true, Deadline: time.Now().Add(time.Hour)}, EffectFunc(func(ctx context.Context, _ WorkerToken) EffectResult {
					entered <- struct{}{}
					<-ctx.Done()
					canceled <- struct{}{}
					<-release
					return EffectResult{}
				}))
				return result
			})
		}()
	}
	for i := 0; i < defaultHerdrCapacity; i++ {
		dispatcherAwait(t, entered)
	}
	acknowledged, returned := make(chan struct{}), make(chan struct{})
	var ran atomic.Bool
	callers.Add(1)
	go func() {
		defer callers.Done()
		defer close(returned)
		d.HandleTopologyAdmitted(context.Background(), nil, func(ctx context.Context) *CommandResult {
			result, _ := d.scheduler.ExecuteAdmitted(ctx, ScheduleOptions{RelayLevel: true, Deadline: time.Now().Add(time.Hour)}, EffectFunc(func(context.Context, WorkerToken) EffectResult { ran.Store(true); return EffectResult{} }), func() { close(acknowledged) })
			return result
		})
	}()
	dispatcherAwait(t, acknowledged)
	began := dispatcherObserve(&observers, func() { _ = d.BeginInstalledDrain() })
	dispatcherAwait(t, began)
	for i := 0; i < defaultHerdrCapacity; i++ {
		dispatcherAwait(t, canceled)
	}
	dispatcherAwait(t, returned)
	if d.InstalledDrainComplete() {
		t.Fatal("completion abandoned actual held effects")
	}
	unlock()
	callers.Wait()
	if err := d.DrainInstalled(); err != nil {
		t.Fatal(err)
	}
	if ran.Load() {
		t.Fatal("queued effect dispatched after cancellation")
	}
}
