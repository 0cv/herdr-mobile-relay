package coordinator

import (
	"context"
	"errors"
	"log/slog"
	"sync"
)

var errNotInstalledScheduler = errors.New("scheduler has no installed ownership")
var errInstalledSchedulerContext = errors.New("installed scheduler requires a context")

type installedScheduler struct {
	mu              sync.Mutex
	lifetime        context.Context
	cancel          context.CancelFunc
	stop            chan struct{}
	done            chan struct{}
	calls           sync.WaitGroup
	effects         sync.WaitGroup
	result          error
	afterCompletion func() // private, configured before construction; before deferred cancel
}

func newInstalledScheduler(capacity int, logger *slog.Logger) *Scheduler {
	return newScheduler(capacity, logger, &installedScheduler{stop: make(chan struct{}), done: make(chan struct{})})
}

func (s *Scheduler) admitInstalled() bool {
	if s.installed == nil {
		return true
	}
	s.installed.mu.Lock()
	defer s.installed.mu.Unlock()
	if s.closed.Load() {
		return false
	}
	s.installed.calls.Add(1)
	return true
}
func installedContextError(ctx context.Context) error {
	if ctx == nil {
		return errInstalledSchedulerContext
	}
	return ctx.Err()
}

// Only admission/bookkeeping is fenced. Launch and callbacks run outside mu.
// The sole owner launches admitted work; the supervisor waits for owner exit.
func (s *Scheduler) admitInstalledEffect() bool {
	if s.installed == nil {
		return true
	}
	s.installed.mu.Lock()
	defer s.installed.mu.Unlock()
	if s.closed.Load() {
		return false
	}
	s.installed.effects.Add(1)
	return true
}

func (s *Scheduler) releaseInstalled() {
	if s.installed != nil {
		s.installed.calls.Done()
	}
}
func (s *Scheduler) beginInstalledDrain() error {
	if s == nil || s.installed == nil {
		return errNotInstalledScheduler
	}
	s.installed.mu.Lock()
	if !s.closed.Swap(true) {
		close(s.installed.stop)
	}
	s.installed.mu.Unlock()
	// Cancellation must progress even while the owner is inside a finalizer.
	s.installed.cancel()
	return nil
}
func (s *Scheduler) finishInstalled() {
	<-s.done // sole launcher has exited: no Add can race with effects.Wait
	s.installed.effects.Wait()
	s.installed.calls.Wait()
	close(s.installed.done)
}
func (s *Scheduler) drainInstalled() error {
	if err := s.beginInstalledDrain(); err != nil {
		return err
	}
	<-s.installed.done
	return s.installed.result
}
func (s *Scheduler) closeInstalled(ctx context.Context) error {
	if ctx == nil {
		return errInstalledSchedulerContext
	}
	if err := s.beginInstalledDrain(); err != nil {
		return err
	}
	select {
	case <-s.installed.done:
		return s.installed.result
	case <-ctx.Done():
		select {
		case <-s.installed.done:
			return s.installed.result
		default:
			return ctx.Err()
		}
	}
}
