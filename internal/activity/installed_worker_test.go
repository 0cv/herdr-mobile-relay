package activity

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestInstalledActivityCanceledCloseFIFO(t *testing.T) {
	j, err := OpenJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := NewInstalledWorker(j)
	ctx, cancel := context.WithCancel(context.Background())
	j.mu.Lock()
	var release sync.Once
	unlock := func() { release.Do(j.mu.Unlock) }
	var callers sync.WaitGroup
	var lateCaller sync.WaitGroup
	lateCtx, lateCancel := context.WithCancel(context.Background())
	observed := make(chan struct{})
	accepted := make(chan uint64, 3)
	w.installed.accepted = func(id uint64) { accepted <- id }
	defer func() {
		cancel()
		lateCancel()
		unlock()
		callers.Wait()
		lateCaller.Wait()
		<-observed
		_ = w.DrainInstalled()
	}()
	go func() { <-ctx.Done(); callers.Wait(); close(observed) }()
	// Hold the actual persistence lock, not a fabricated completion event.
	for i := 0; i < 3; i++ {
		callers.Add(1)
		go func(i int) {
			defer callers.Done()
			_, _ = w.Commit(ctx, ActivityCommitRequested{Sequence: uint64(i), Entry: Entry{ID: fmt.Sprint(i), Kind: "test"}})
		}(i)
		select {
		case id := <-accepted:
			if id != uint64(i) {
				t.Fatal("enqueue order")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("enqueue not acknowledged")
		}
	}
	cancel()
	select {
	case <-observed:
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation blocked while journal held")
	}
	if err := w.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close: %v", err)
	}
	late := make(chan error, 1)
	lateCaller.Add(1)
	go func() {
		defer lateCaller.Done()
		_, err := w.Commit(lateCtx, ActivityCommitRequested{})
		late <- err
	}()
	select {
	case err := <-late:
		if !errors.Is(err, ErrInstalledWorkerClosed) {
			t.Fatalf("late admission: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("late refusal blocked while journal held")
	}
	select {
	case <-w.installed.done:
		t.Fatal("drained while journal held")
	default:
	}
	unlock()
	if err := w.DrainInstalled(); err != nil {
		t.Fatal(err)
	}
	if err := w.DrainInstalled(); err != nil {
		t.Fatal(err)
	}
	j.mu.RLock()
	defer j.mu.RUnlock()
	for i, entry := range j.entries {
		if entry.ID != fmt.Sprint(i) {
			t.Fatalf("FIFO: %v", j.entries)
		}
	}
	if len(j.entries) != 3 {
		t.Fatalf("accepted writes lost: %d", len(j.entries))
	}
}

func TestInstalledActivityBoundedAcceptedErrors(t *testing.T) {
	j, err := OpenJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := NewInstalledWorker(j)
	defer w.DrainInstalled()
	// JSON encoding fails inside the actual accepted journal operation. No FD
	// manipulation, native fault injection, or external client is involved.
	for i := 0; i < installedErrorWitnessLimit+3; i++ {
		_, err := w.Commit(context.Background(), ActivityCommitRequested{Entry: Entry{Details: map[string]any{"unsupported": make(chan int)}}})
		if err == nil {
			t.Fatal("expected journal encoding error")
		}
	}
	first := w.DrainInstalled()
	var overflow *InstalledErrorOverflow
	if !errors.As(first, &overflow) || overflow.Count != 3 {
		t.Fatalf("overflow: %v", first)
	}
	if second := w.DrainInstalled(); second != first {
		t.Fatal("result not cached")
	}
	if len(w.installed.witnesses) != installedErrorWitnessLimit+1 {
		t.Fatal("unbounded witness retention")
	}
}

func TestInstalledActivityRejectsUnownedDrain(t *testing.T) {
	var w Worker
	if !errors.Is(w.DrainInstalled(), ErrNotInstalledWorker) {
		t.Fatal("zero worker claimed installed ownership")
	}
}
