package app

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type uploadObserverResource struct {
	begin func() error
	drain func() error
}

func (r *uploadObserverResource) BeginInstalledDrain() error { return r.begin() }
func (r *uploadObserverResource) DrainInstalled() error      { return r.drain() }

func TestInstalledUploadObserverStopped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int32
	owner := &installedUploadOwner{manager: &uploadObserverResource{
		begin: func() error { calls.Add(1); return nil },
		drain: func() error { return nil },
	}}
	observer := observeInstalledUpload(ctx, owner)
	// This successful stop is the actual AfterFunc exclusion receipt, not a
	// scheduler observation. Do not stop twice: a second stop means join.
	t.Cleanup(cancel)
	observer.stopOrJoin()
	cancel()
	if calls.Load() != 0 {
		t.Fatal("stopped callback ran")
	}
	if err := owner.collectResult(); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("terminal fence missing")
	}
}

func TestInstalledUploadObserverHeld(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	entered, hold := make(chan struct{}), make(chan struct{})
	waiting, returned := make(chan struct{}), make(chan struct{})
	released := make(chan struct{})
	var release sync.Once
	var callers sync.WaitGroup
	owner := &installedUploadOwner{manager: &uploadObserverResource{
		begin: func() error { close(entered); <-hold; return nil },
		drain: func() error { close(released); return nil },
	}}
	observer := observeInstalledUpload(ctx, owner)
	observer.waiting = func() { close(waiting) }
	// Even a missing receipt releases the callback before joining it. No
	// synchronous fence can precede release here: begin itself is the holder.
	t.Cleanup(func() {
		cancel()
		release.Do(func() { close(hold) })
		callers.Wait()
		<-observer.done
		_ = owner.collectResult()
	})
	cancel()
	awaitUploadControlReceipt(t, entered)
	callers.Add(1)
	go func() {
		defer callers.Done()
		observer.stopOrJoin()
		_ = owner.collectResult()
		close(returned)
	}()
	awaitUploadControlReceipt(t, waiting)
	if ctx.Err() != context.Canceled {
		t.Fatal("cancellation missing")
	}
	timer := time.NewTimer(20 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-returned:
		t.Fatal("observer join returned while callback held")
	case <-released:
		t.Fatal("physical release before observer join")
	case <-timer.C:
	}
	release.Do(func() { close(hold) })
	callers.Wait()
	<-returned
	<-released
}

func TestInstalledUploadOwnerConcurrentErrorBranches(t *testing.T) {
	beginErr, drainErr := errors.New("begin"), errors.New("drain")
	primary := errors.New("primary")
	controlErr := errors.New("control")
	ctx, cancel := context.WithCancel(context.Background())
	controlHeld, controlDone := make(chan struct{}), make(chan struct{})
	controlWaiting := make(chan struct{})
	var release sync.Once
	var callers sync.WaitGroup
	var begins, drains atomic.Int32
	owner := &installedUploadOwner{manager: &uploadObserverResource{
		begin: func() error { begins.Add(1); return beginErr },
		drain: func() error { drains.Add(1); return drainErr },
	}}
	owner.bindControl(cancel, controlDone)
	results := make(chan error, 4)
	t.Cleanup(func() {
		owner.begin()
		cancel()
		release.Do(func() { close(controlHeld) })
		callers.Wait()
	})
	callers.Add(1)
	go func() {
		defer callers.Done()
		defer close(controlDone)
		<-ctx.Done()
		close(controlWaiting)
		<-controlHeld
	}()
	for i := 0; i < 4; i++ {
		callers.Add(1)
		go func() {
			defer callers.Done()
			// Equal independent primary/control siblings must not be deduplicated
			// against the owner's cached physical witness.
			results <- errors.Join(primary, controlErr, drainErr, owner.collectResult())
		}()
	}
	awaitUploadControlReceipt(t, controlWaiting)
	timer := time.NewTimer(20 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-results:
		t.Fatal("cleanup escaped held control")
	case <-timer.C:
	}
	if drains.Load() != 0 {
		t.Fatal("physical drain before control join")
	}
	release.Do(func() { close(controlHeld) })
	callers.Wait()
	close(results)
	var branches []error
	for err := range results {
		branches = append(branches, err)
	}
	final := errors.Join(branches...)
	if len(branches) != 4 || uploadObserverLeaves(final, beginErr) != 1 || uploadObserverLeaves(final, drainErr) != 5 || uploadObserverLeaves(final, primary) != 4 || uploadObserverLeaves(final, controlErr) != 4 {
		t.Fatal("lost or duplicated independent result branches", final)
	}
	if begins.Load() != 1 || drains.Load() != 1 {
		t.Fatal("repeated resource operation")
	}
	if owner.collectResult() != nil {
		t.Fatal("recollected resource result")
	}
	cached := owner.drain()
	if uploadObserverLeaves(cached, beginErr) != 1 || uploadObserverLeaves(cached, drainErr) != 1 {
		t.Fatal("cached drain lost siblings")
	}
}

func uploadObserverLeaves(err, target error) int {
	if err == target {
		return 1
	}
	n := 0
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			n += uploadObserverLeaves(child, target)
		}
	}
	return n
}
