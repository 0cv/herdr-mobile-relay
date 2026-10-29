package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/upload"
)

func TestInstalledUploadActualFactoryOutcomes(t *testing.T) {
	for _, fatal := range []bool{false, true} {
		name := "optional"
		if fatal {
			name = "fatal"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			cfg := upload.Config{Root: root}
			var mutationErr error
			var once sync.Once
			if fatal {
				cfg.Now = func() time.Time {
					once.Do(func() { mutationErr = os.Remove(filepath.Join(root, "objects")) })
					return time.Now()
				}
			} else {
				// Failure after root acquisition, before any clock callback or sweep.
				if err := os.WriteFile(filepath.Join(root, "sessions"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			manager, owner, err := acquireAppUpload(cliInstalledUploadResources, cfg)
			// Register even on unexpected success/classification regression.
			registerInstalledUploadFixtureCleanup(t, manager, owner)
			if mutationErr != nil {
				t.Fatal(mutationErr)
			}
			if manager != nil || owner == nil || err == nil {
				t.Fatalf("factory result: manager=%v owner=%v err=%v", manager != nil, owner != nil, err)
			}
			if (fatalUploadConstruction(err) != nil) != fatal {
				t.Fatalf("classification: %v", err)
			}
			if fatal {
				var pathErr *os.PathError
				if !errors.As(err, &pathErr) {
					t.Fatal("lost actual filesystem witness")
				}
			}
			if owner.result(nil) != nil {
				t.Fatal("absent manager fabricated cleanup")
			}
		})
	}
}

// No constructor is executed: this checks the actual factory selection with
// invalid input before filesystem acquisition. Call-site binding is source-only.
func TestInstalledUploadPreAcquisitionPolicyOutcomes(t *testing.T) {
	for _, policy := range []uploadResourcePolicy{ordinaryUploadResources, cliInstalledUploadResources} {
		manager, owner, err := acquireAppUpload(policy, upload.Config{})
		registerInstalledUploadFixtureCleanup(t, manager, owner)
		if manager != nil || err == nil || (owner != nil) != (policy == cliInstalledUploadResources) {
			t.Fatal("resource selection changed")
		}
	}
}

func uploadOutcomeLeaves(err, witness error) int {
	if err == witness {
		return 1
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		n := 0
		for _, child := range joined.Unwrap() {
			n += uploadOutcomeLeaves(child, witness)
		}
		return n
	}
	return 0
}

func TestInstalledUploadSharedResultOutcomes(t *testing.T) {
	for _, name := range []string{"init", "generation", "binding", "run", "control", "cancel", "equal-siblings"} {
		t.Run(name, func(t *testing.T) {
			physical := errors.New("physical")
			primary := errors.New(name)
			control := errors.New("control")
			if name == "cancel" {
				primary = context.Canceled
			}
			if name == "equal-siblings" {
				primary, control = physical, physical
			}
			owner := &installedUploadOwner{manager: &uploadOwnerLogicalResource{err: physical}}
			t.Cleanup(func() { _ = owner.drain() })
			// The actual Run defer and outer deferred wrapper use this method.
			result := owner.result(primary, control)
			result = owner.result(result)
			want := 1
			if name == "equal-siblings" {
				want = 3
			}
			if uploadOutcomeLeaves(result, physical) != want || !errors.Is(result, primary) || !errors.Is(result, control) {
				t.Fatal("result siblings lost/duplicated")
			}
			if owner.drain() != physical {
				t.Fatal("cached error lost")
			}
		})
	}
}

type uploadOutcomeHeldResource struct {
	work, joining, released chan struct{}
	err                     error
}

func (r *uploadOutcomeHeldResource) BeginInstalledDrain() error { return nil }
func (r *uploadOutcomeHeldResource) DrainInstalled() error {
	close(r.joining)
	<-r.work
	close(r.released)
	return r.err
}

func TestInstalledUploadHeldResultOutcomes(t *testing.T) {
	for _, name := range []string{"runner-error", "cancellation"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			controlHold, controlWaiting, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			r := &uploadOutcomeHeldResource{work: make(chan struct{}), joining: make(chan struct{}), released: make(chan struct{}), err: errors.New("resource")}
			owner := &installedUploadOwner{manager: r}
			workHold, workEntered := make(chan struct{}), make(chan struct{})
			var controlRelease, workRelease sync.Once
			var callers sync.WaitGroup
			result := make(chan error, 1)
			primary, controlErr := errors.New("runner"), errors.New("control")
			if name == "cancellation" {
				primary = context.Canceled
			}
			t.Cleanup(func() {
				owner.begin()
				cancel()
				controlRelease.Do(func() { close(controlHold) })
				workRelease.Do(func() { close(workHold) })
				callers.Wait()
				_ = owner.drain()
			})
			owner.bindControl(cancel, done)
			callers.Add(1)
			go func() {
				defer callers.Done()
				defer close(r.work)
				close(workEntered) // logical admitted-work boundary, before held work
				<-workHold
			}()
			callers.Add(2)
			go func() {
				defer callers.Done()
				defer close(done)
				<-ctx.Done()
				close(controlWaiting)
				<-controlHold
			}()
			go func() { defer callers.Done(); result <- owner.result(primary, controlErr) }()
			// All bound participants are running before any fallible receipt.
			awaitUploadControlReceipt(t, workEntered)
			awaitUploadControlReceipt(t, controlWaiting)
			negative := func() {
				t.Helper()
				timer := time.NewTimer(20 * time.Millisecond)
				defer timer.Stop()
				select {
				case <-r.released:
					t.Fatal("physical release while holder live")
				case <-result:
					t.Fatal("result while holder live")
				case <-timer.C:
				}
			}
			negative()
			controlRelease.Do(func() { close(controlHold) })
			awaitUploadControlReceipt(t, r.joining)
			negative()
			workRelease.Do(func() { close(workHold) })
			callers.Wait()
			outer := owner.result(<-result)
			for _, err := range []error{primary, controlErr, r.err} {
				if uploadOutcomeLeaves(outer, err) != 1 {
					t.Fatal("missing/duplicate outcome")
				}
			}
		})
	}
}
