package coordinator

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestInstalledDispatcherDrainCompleteDiagnostic(t *testing.T) {
	var absent *Dispatcher
	var zero Dispatcher
	if absent.InstalledDrainComplete() || zero.InstalledDrainComplete() {
		t.Fatal("non-installed completion")
	}
	d := dispatcherFixture(t, nil)
	var release func()
	var observers sync.WaitGroup
	defer func() {
		dispatcherStopForCleanup(d)
		if release != nil {
			release()
		}
		observers.Wait()
		d.DrainInstalled()
	}()
	ordinary := NewDispatcher(nil, d.state, nil, d.logger)
	defer ordinary.Close(context.Background())
	if ordinary.InstalledDrainComplete() || d.InstalledDrainComplete() {
		t.Fatal("early completion")
	}
	ctx, leave, err := d.enterInstalled(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release = func() { once.Do(leave) }
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	var closeErr error
	closed := dispatcherObserve(&observers, func() { closeErr = d.Close(canceled) })
	dispatcherAwait(t, closed)
	if !errors.Is(closeErr, context.Canceled) {
		t.Fatalf("held Close: %v", closeErr)
	}
	dispatcherAwait(t, ctx.Done())
	if d.InstalledDrainComplete() {
		t.Fatal("diagnostic hid held caller")
	}
	release()
	// Close, not a test's repairing DrainInstalled, must establish completion.
	if err := d.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !d.InstalledDrainComplete() {
		t.Fatal("Close did not publish full completion")
	}
	if ordinary.InstalledDrainComplete() {
		t.Fatal("observation changed ordinary ownership")
	}
}
