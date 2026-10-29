package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type uploadControlReleaseWitness struct {
	once     sync.Once
	released chan struct{}
	err      error
}

func (r *uploadControlReleaseWitness) BeginInstalledDrain() error { return nil }
func (r *uploadControlReleaseWitness) DrainInstalled() error {
	r.once.Do(func() { close(r.released) })
	return r.err
}

func TestInstalledUploadControlBeforeRelease(t *testing.T) {
	for _, mode := range []string{"setup", "strict", "absent-manager"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			hold := make(chan struct{})
			entered := make(chan struct{})
			waiting := make(chan struct{})
			done := make(chan struct{})
			returned := make(chan error, 1)
			var release sync.Once
			var callers sync.WaitGroup
			witness := errors.New("physical release")
			r := &uploadControlReleaseWitness{released: make(chan struct{}), err: witness}
			o := &installedUploadOwner{}
			if mode != "absent-manager" {
				o.manager = r
			}
			// Install cleanup before starting either caller. A failed bounded
			// observation always releases the holder and joins both callers.
			t.Cleanup(func() {
				o.begin()
				cancel()
				release.Do(func() { close(hold) })
				callers.Wait()
			})
			o.bindControl(cancel, done)
			callers.Add(1)
			go func() {
				defer callers.Done()
				defer close(done)
				close(entered) // actual callback entry
				<-ctx.Done()
				close(waiting) // cancellation received at internal held boundary
				<-hold
			}()
			awaitUploadControlReceipt(t, entered)
			callers.Add(1)
			go func() { defer callers.Done(); returned <- o.collectResult() }()
			awaitUploadControlReceipt(t, waiting)
			timer := time.NewTimer(20 * time.Millisecond)
			defer timer.Stop()
			select {
			case <-returned:
				t.Fatal("drain returned with control held")
			case <-r.released:
				t.Fatal("physical release preceded control join")
			case <-timer.C:
			}
			release.Do(func() { close(hold) })
			callers.Wait()
			err := <-returned
			if mode == "absent-manager" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err != witness {
				t.Fatal("lost physical result", err)
			}
			if err := o.collectResult(); err != nil {
				t.Fatal("duplicate collection", err)
			}
		})
	}
}

func awaitUploadControlReceipt(t *testing.T, receipt <-chan struct{}) {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-receipt:
	case <-timer.C:
		t.Fatal("missing control boundary receipt")
	}
}

func TestInstalledUploadControlReturnKeepsService(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan struct{})
	o := &installedUploadOwner{}
	join := o.bindControl(cancel, done)
	close(done) // callback completion alone has no service-lifetime effect
	if ctx.Err() != nil {
		t.Fatal("callback stopped service")
	}
	join()
	if ctx.Err() != context.Canceled {
		t.Fatal("shutdown did not cancel")
	}
}
