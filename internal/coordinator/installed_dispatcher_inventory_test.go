package coordinator

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/profiles"
)

type dispatcherLogWriter func([]byte) (int, error)

func (w dispatcherLogWriter) Write(p []byte) (int, error) { return w(p) }

func TestInstalledDispatcherInventoryCallbackFenceAndBoundedErrors(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unlock := func() { once.Do(func() { close(release) }) }
	var logged atomic.Int32
	var observers sync.WaitGroup
	var d *Dispatcher
	logger := slog.New(slog.NewTextHandler(dispatcherLogWriter(func(p []byte) (int, error) {
		if strings.Contains(string(p), "profile ownership reconciliation failed") && logged.Add(1) == 18 {
			began := dispatcherObserve(&observers, func() { _ = d.BeginInstalledDrain() })
			select {
			case <-began:
				close(entered)
			case <-release:
				return len(p), nil
			}
			<-release
		}
		return len(p), nil
	}), nil))
	d = NewInstalledDispatcher(nil, NewState(logger), nil, logger)
	var callers sync.WaitGroup
	defer func() { dispatcherStopForCleanup(d); unlock(); callers.Wait(); observers.Wait(); d.DrainInstalled() }()
	resolver := profiles.NewResolver(t.TempDir(), nil)
	d.SetProfiles(resolver)
	d.state.mu.Lock()
	callback := d.state.onInventory
	d.state.mu.Unlock()
	if callback == nil {
		t.Fatal("inventory callback not installed")
	}
	// Invalid identities are independently refused in ReconcileOwnership before
	// persistence. No client or discovery operation can occur if the fence fails.
	for i := 0; i < 17; i++ {
		callback([]*AgentState{{}})
	}
	callers.Add(1)
	go func() { defer callers.Done(); callback([]*AgentState{{}}) }()
	dispatcherAwait(t, entered)
	if d.InstalledDrainComplete() {
		t.Fatal("callback not counted")
	}
	unlock()
	callers.Wait()
	err := d.Close(context.Background())
	var overflow *InstalledDispatcherErrorOverflow
	if !errors.As(err, &overflow) || overflow.Count != 2 {
		t.Fatalf("unbounded or lost ownership errors: %v", err)
	}
	callback([]*AgentState{{}})
	if logged.Load() != 18 {
		t.Fatal("retired inventory callback ran")
	}
	d.SetProfiles(nil)
	if d.profiles != resolver {
		t.Fatal("retired profile setter ran")
	}
	if !d.InstalledDrainComplete() {
		t.Fatal("cached error completion not observed")
	}
}
