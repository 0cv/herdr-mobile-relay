package coordinator

import (
	"context"
	"sync"
	"testing"
)

func TestInstalledDispatcherDeferredAckBeforeScopeRelease(t *testing.T) {
	d := dispatcherFixture(t, nil)
	var observers sync.WaitGroup
	var scopeCtx context.Context
	entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(returned)
		defer func() {
			if recover() == nil {
				t.Error("missing handler panic")
			}
		}()
		d.HandleTopologyAdmitted(context.Background(), func() {
			_, done, err := d.enterInstalled(scopeCtx)
			if err != nil {
				t.Error("scope released before deferred acknowledgement", err)
			} else {
				done()
			}
			began := dispatcherObserve(&observers, func() { _ = d.BeginInstalledDrain() })
			select {
			case <-began:
				close(entered)
			case <-release:
				return
			}
			<-release
		}, func(ctx context.Context) *CommandResult {
			scopeCtx = ctx
			panic("pure fixture unwind")
		})
	}()
	defer func() {
		dispatcherStopForCleanup(d)
		close(release)
		<-returned
		observers.Wait()
		_ = d.DrainInstalled()
	}()
	dispatcherAwait(t, entered)
	if d.InstalledDrainComplete() {
		t.Error("drained during deferred acknowledgement")
	}
}

func TestInstalledDispatcherHeldForeignScopeRefusalLifetime(t *testing.T) {
	ctx := context.WithValue(context.Background(), dispatcherScopeKey{}, &dispatcherScope{owner: &installedDispatcher{}, active: true})
	heldInstalledRefusal(t, ctx, false)
	heldInstalledRefusal(t, ctx, true)
}
