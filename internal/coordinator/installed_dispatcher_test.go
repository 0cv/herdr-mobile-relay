package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/activity"
)

func dispatcherFixture(t *testing.T, journal *activity.Journal) *Dispatcher {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewInstalledDispatcher(nil, NewState(logger), journal, logger)
}
func dispatcherAwait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("dispatcher event did not arrive")
	}
}

func TestInstalledDispatcherConstructionAndOrdinaryIsolation(t *testing.T) {
	d := dispatcherFixture(t, nil)
	defer d.DrainInstalled()
	if d.installed == nil || d.scheduler.installed == nil {
		t.Fatal("ordinary worker graph")
	}
	var zero Dispatcher
	if !errors.Is(zero.BeginInstalledDrain(), ErrNotInstalledDispatcher) || !errors.Is(zero.DrainInstalled(), ErrNotInstalledDispatcher) {
		t.Fatal("zero drain accepted")
	}
	ordinary := NewDispatcher(nil, d.state, nil, d.logger)
	defer ordinary.Close(context.Background())
	if !errors.Is(ordinary.DrainInstalled(), ErrNotInstalledDispatcher) {
		t.Fatal("ordinary converted")
	}
	if ordinary.Handle(context.Background(), map[string]any{"action": "invalid"}).Error != "Unknown command" {
		t.Fatal("ordinary dispatch changed")
	}
	if !errors.Is(d.Close(nil), ErrInstalledDispatcherContext) {
		t.Fatal("nil Close accepted")
	}
	if d.Handle(context.Background(), map[string]any{"action": "invalid"}).Error != "Unknown command" {
		t.Fatal("nil Close stopped dispatcher")
	}
}

func TestInstalledDispatcherDirectRefusalsAndAcknowledgements(t *testing.T) {
	d := dispatcherFixture(t, nil)
	defer d.DrainInstalled()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, ctx := range []context.Context{nil, canceled} {
		ack := 0
		if d.HandleAdmitted(ctx, map[string]any{"action": "invalid"}, func() { ack++ }).OK || ack != 1 {
			t.Fatal("refusal acknowledgement")
		}
		// The callback is logical only, even if the admission guard regresses.
		ran := false
		ack = 0
		d.HandleTopologyAdmitted(ctx, func() { ack++ }, func(context.Context) *CommandResult { ran = true; return &CommandResult{} })
		if ran || ack != 1 {
			t.Fatal("topology refusal")
		}
	}
	if err := d.BeginInstalledDrain(); err != nil {
		t.Fatal(err)
	}
	// All requests are independently invalid before any Herdr path. Nil client
	// is not the denial mechanism: empty targets/labels/selection are.
	calls := []func() *CommandResult{
		func() *CommandResult { return d.Handle(context.Background(), map[string]any{"action": "invalid"}) },
		func() *CommandResult { return d.HandleWorkspaceCreate(context.Background(), "", "", "") },
		func() *CommandResult { return d.HandleWorkspaceRename(context.Background(), "", "", "") },
		func() *CommandResult { return d.HandleWorkspaceReorder(context.Background(), "", "", nil) },
		func() *CommandResult { return d.HandleWorkspaceReorderBlock(context.Background(), "", nil, "") },
		func() *CommandResult { return d.HandleWorkspaceClose(context.Background(), "", "", false, nil) },
		func() *CommandResult { return d.HandleWorktreeList(context.Background(), "", "") },
		func() *CommandResult { return d.HandleWorktreeCreate(context.Background(), "", "", "", "", "", "") },
		func() *CommandResult { return d.HandleWorktreeOpen(context.Background(), "", "", "", "", "") },
		func() *CommandResult { return d.HandleWorktreeRemove(context.Background(), "", "", false) },
	}
	for i, call := range calls {
		if got := call(); got.Error != ErrInstalledDispatcherClosed.Error() {
			t.Fatalf("entry %d: %v", i, got)
		}
	}
	for _, call := range []func(context.Context, map[string]any) map[string]any{d.HandleReadPane, d.HandleProbePane} {
		if got := call(context.Background(), map[string]any{}); got["error"] != ErrInstalledDispatcherClosed.Error() {
			t.Fatal("read/probe not fenced")
		}
	}
	delivered := 0
	if d.HandleClearActivities("", func(*CommandResult) { delivered++ }).Error != ErrInstalledDispatcherClosed.Error() || delivered != 1 {
		t.Fatal("clear refusal")
	}
	d.RecordActivity("logical", "", "", "", "")
	if d.RecordTransitionActivity("working", "", "", "", "", 0, nil, "", "", "", "", "") {
		t.Fatal("transition admitted")
	}
	d.PruneSlots(nil)
	d.ApplyTopology(nil, nil)
	d.CancelInflight()
	if d.startWatcher(func(context.Context) {}) {
		t.Fatal("late watcher")
	}
	d.SetBroadcast(func(any) { t.Error("late setter") })
	d.SetWakePoll(func() { t.Error("late setter") })
	if d.broadcast != nil || d.wakePoll != nil {
		t.Fatal("retired construction setter")
	}
}

func TestInstalledDispatcherNestedScopeAuthority(t *testing.T) {
	d := dispatcherFixture(t, nil)
	other := dispatcherFixture(t, nil)
	defer d.DrainInstalled()
	defer other.DrainInstalled()
	ctx, release, err := d.enterInstalled(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	finish := func() { once.Do(release) }
	defer finish()
	child, childRelease, err := d.enterInstalled(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var childOnce sync.Once
	finishChild := func() { childOnce.Do(childRelease) }
	var observers sync.WaitGroup
	defer func() { dispatcherStopForCleanup(d); finish(); finishChild(); observers.Wait() }()
	if _, leave, err := other.enterInstalled(ctx); err == nil {
		leave()
		t.Fatal("foreign scope accepted")
	}
	if got := d.Handle(ctx, map[string]any{"action": "invalid"}); got.Error != "Unknown command" {
		t.Fatal(got)
	}
	var retained context.Context
	acks := 0
	result := d.HandleTopologyAdmitted(ctx, func() { acks++ }, func(nested context.Context) *CommandResult {
		retained = nested
		return d.HandleAdmitted(nested, map[string]any{"action": "invalid"}, func() { acks++ })
	})
	if result.Error != "Unknown command" || acks != 2 {
		t.Fatal("nested public delegation or acknowledgement")
	}
	if got := d.Handle(retained, map[string]any{"action": "invalid"}); got.Error == "Unknown command" {
		t.Fatal("retained callback scope bypassed admission")
	}
	finish()
	if _, leave, err := d.enterInstalled(ctx); err == nil {
		leave()
		t.Fatal("retained scope accepted")
	}
	if child.Err() != context.Canceled {
		t.Fatal("nested cancellation not inherited")
	}
	began := dispatcherObserve(&observers, func() { _ = d.BeginInstalledDrain() })
	dispatcherAwait(t, began)
	select {
	case <-d.installed.done:
		t.Fatal("borrowed call not counted")
	default:
	}
	finishChild()
	if err := d.DrainInstalled(); err != nil {
		t.Fatal(err)
	}
}

func TestInstalledDispatcherReentrantWriterAndWatcherOrdering(t *testing.T) {
	journal, err := activity.OpenJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d := dispatcherFixture(t, journal)
	entered, release, watchCanceled := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unlock := func() { once.Do(func() { close(release) }) }
	var callers sync.WaitGroup
	var observers sync.WaitGroup
	defer func() {
		dispatcherStopForCleanup(d)
		unlock()
		callers.Wait()
		d.watcherWG.Wait()
		observers.Wait()
		d.DrainInstalled()
	}()
	if !d.startWatcher(func(ctx context.Context) {
		<-ctx.Done()
		close(watchCanceled)
		<-release
		// Private finalization remains available after the admission fence.
		d.recordActivity("watcher", "done", "joined", "", "")
	}) {
		t.Fatal("watcher refused")
	}
	var broadcasts atomic.Int32
	d.SetBroadcast(func(any) {
		if broadcasts.Add(1) != 1 {
			return
		}
		began := dispatcherObserve(&observers, func() { _ = d.BeginInstalledDrain() })
		select {
		case <-began:
			close(entered) // Positive proof: Begin returned before holder release.
		case <-release: // Failure cleanup must not depend on Begin returning.
			return
		}
		<-release
	})
	callers.Add(1)
	go func() { defer callers.Done(); d.RecordActivity("writer", "done", "held", "", "") }()
	dispatcherAwait(t, entered)
	dispatcherAwait(t, watchCanceled)
	select {
	case <-d.installed.done:
		t.Fatal("held writer drained")
	default:
	}
	unlock()
	callers.Wait()
	if err := d.DrainInstalled(); err != nil {
		t.Fatal(err)
	}
	if got := len(journal.Recent(10)); got != 2 {
		t.Fatalf("lost admitted persistence: %d", got)
	}
}

func TestInstalledDispatcherSchedulerStopsBeforeHeldCallers(t *testing.T) {
	d := dispatcherFixture(t, nil)
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unlock := func() { once.Do(func() { close(release) }) }
	var callers sync.WaitGroup
	ctx, leave, err := d.enterInstalled(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var leaveOnce sync.Once
	finish := func() { leaveOnce.Do(leave) }
	var observers sync.WaitGroup
	defer func() {
		dispatcherStopForCleanup(d)
		unlock()
		finish()
		callers.Wait()
		observers.Wait()
		d.DrainInstalled()
	}()
	callers.Add(1)
	go func() {
		defer callers.Done()
		d.scheduler.Execute(ctx, ScheduleOptions{RelayLevel: true, Deadline: time.Now().Add(time.Hour)}, EffectFunc(func(ctx context.Context, _ WorkerToken) EffectResult {
			close(entered)
			<-ctx.Done()
			close(canceled)
			<-release
			return EffectResult{}
		}))
	}()
	dispatcherAwait(t, entered)
	began := dispatcherObserve(&observers, func() { _ = d.BeginInstalledDrain() })
	dispatcherAwait(t, began)
	dispatcherAwait(t, canceled)
	select {
	case <-d.installed.done:
		t.Fatal("effect or caller abandoned")
	default:
	}
	unlock()
	callers.Wait()
	finish()
	if err := d.DrainInstalled(); err != nil {
		t.Fatal(err)
	}
}

func TestInstalledDispatcherConcurrentCachedPersistenceErrors(t *testing.T) {
	dir := t.TempDir()
	journal, err := activity.OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	d := dispatcherFixture(t, journal)
	defer d.DrainInstalled()
	// Real private-FS failure, no preclosed descriptors or external effects.
	if err := os.Mkdir(filepath.Join(dir, "activity.jsonl"), 0700); err != nil {
		t.Fatal(err)
	}
	d.RecordActivity("logical", "done", "", "", "")
	d.RecordTransitionActivity("working", "done", "", "", "", 0, map[string]any{"invalid": make(chan int)}, "", "", "", "", "")
	const n = 8
	results := make(chan error, n)
	var callers sync.WaitGroup
	defer callers.Wait()
	for i := 0; i < n; i++ {
		callers.Add(1)
		go func(i int) {
			defer callers.Done()
			if i%2 == 0 {
				results <- d.DrainInstalled()
			} else {
				results <- d.Close(context.Background())
			}
		}(i)
	}
	callers.Wait()
	close(results)
	var first error
	for result := range results {
		if result == nil {
			t.Fatal("lost persistence failure")
		}
		if first == nil {
			first = result
		} else if result != first {
			t.Fatal("result not cached")
		}
	}
	var pathErr *os.PathError
	var jsonErr *json.UnsupportedTypeError
	if !errors.As(first, &pathErr) || !errors.As(first, &jsonErr) {
		t.Fatalf("independent siblings lost: %v", first)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if d.Close(ctx) != first {
		t.Fatal("ready errors lost to cancellation")
	}
}
