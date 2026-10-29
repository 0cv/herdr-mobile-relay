package activity

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

var ErrInstalledContext = errors.New("installed activity requires a context")

var ErrInstalledWorkerClosed = errors.New("installed activity worker is closed")
var ErrNotInstalledWorker = errors.New("activity worker has no installed ownership")

const installedErrorWitnessLimit = 16

// InstalledErrorOverflow records failures beyond the fixed witness budget.
// Count saturates rather than wrapping; the marker permanently prevents success.
type InstalledErrorOverflow struct{ Count uint64 }

func (e *InstalledErrorOverflow) Error() string {
	return fmt.Sprintf("installed activity error witness overflow: %d", e.Count)
}

type installedWorker struct {
	accepted  func(uint64) // private enqueue observation; configured before submissions
	mu        sync.Mutex
	closing   bool
	calls     sync.WaitGroup
	stop      chan struct{}
	done      chan struct{}
	witnesses []error // written only by the worker, read after its join
	overflow  uint64
	result    error
}

// NewInstalledWorker selects ownership before either owned goroutine starts.
func NewInstalledWorker(journal *Journal) *Worker {
	w := &Worker{journal: journal, requests: make(chan workerRequest, 64), done: make(chan struct{}), installed: &installedWorker{stop: make(chan struct{}), done: make(chan struct{})}}
	go w.run()
	go w.finishInstalled()
	return w
}

func (w *Worker) admitInstalled(ctx context.Context) error {
	if w.installed == nil {
		return nil
	}
	if ctx == nil {
		return ErrInstalledContext
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s := w.installed
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.closing {
		return ErrInstalledWorkerClosed
	}
	s.calls.Add(1)
	return nil
}
func (w *Worker) ackInstalled(sequence uint64) {
	if w.installed != nil && w.installed.accepted != nil {
		w.installed.accepted(sequence)
	}
}

func (w *Worker) releaseInstalled() {
	if w.installed != nil {
		w.installed.calls.Done()
	}
}
func (w *Worker) beginInstalledDrain() {
	s := w.installed
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closing {
		s.closing = true
		close(s.stop)
	}
}
func (w *Worker) finishInstalled() {
	s := w.installed
	defer close(s.done)
	<-s.stop
	// The fence precedes Wait: no Add can race with a zero count.
	// Keep the worker available until every admitted caller has returned.
	s.calls.Wait()
	w.requests <- workerRequest{} // exactly one FIFO sentinel, never cancellable
	<-w.done
	if s.overflow != 0 {
		s.witnesses = append(s.witnesses, &InstalledErrorOverflow{Count: s.overflow})
	}
	s.result = errors.Join(s.witnesses...)
}

// DrainInstalled cannot abandon accepted writes. Close(ctx) shares its stop
// protocol, but may stop waiting without affecting this owner.
func (w *Worker) DrainInstalled() error {
	if w == nil || w.installed == nil {
		return ErrNotInstalledWorker
	}
	w.beginInstalledDrain()
	<-w.installed.done
	return w.installed.result
}
func (w *Worker) retainInstalledError(err error) {
	if w.installed == nil || err == nil {
		return
	}
	s := w.installed
	if len(s.witnesses) < installedErrorWitnessLimit {
		s.witnesses = append(s.witnesses, err)
	} else if s.overflow != ^uint64(0) {
		s.overflow++
	}
}
