package activity

import (
	"context"
	"errors"
)

type ActivityCommitRequested struct {
	Sequence uint64
	Entry    Entry
}

type ActivityCommitted struct {
	Sequence uint64
	Entry    Entry
}

type ActivityCommitFailed struct {
	Sequence uint64
	Err      error
}

type ActivityClearRequested struct {
	Sequence uint64
}

type ActivityCleared struct {
	Sequence uint64
}

type ActivityClearFailed struct {
	Sequence uint64
	Err      error
}

type ActivityDiscardRequested struct {
	Sequence uint64
	ID       string
}

type ActivityDiscarded struct {
	Sequence uint64
	ID       string
}

type ActivityDiscardFailed struct {
	Sequence uint64
	ID       string
	Err      error
}

type workerRequest struct {
	commit  *ActivityCommitRequested
	clear   *ActivityClearRequested
	discard *ActivityDiscardRequested
	reply   chan workerReply
}

type workerReply struct {
	event any
}

type Worker struct {
	installed *installedWorker
	journal   *Journal
	requests  chan workerRequest
	done      chan struct{}
}

func NewWorker(journal *Journal) *Worker {
	worker := &Worker{
		journal:  journal,
		requests: make(chan workerRequest, 64),
		done:     make(chan struct{}),
	}
	go worker.run()
	return worker
}

func (w *Worker) Commit(ctx context.Context, request ActivityCommitRequested) (ActivityCommitted, error) {
	if err := w.admitInstalled(ctx); err != nil {
		return ActivityCommitted{}, err
	}
	defer w.releaseInstalled()
	if w.installed != nil {
		if err := ctx.Err(); err != nil {
			return ActivityCommitted{}, err
		}
	}
	reply := make(chan workerReply, 1)
	select {
	case w.requests <- workerRequest{commit: &request, reply: reply}:
		w.ackInstalled(request.Sequence)
	case <-ctx.Done():
		return ActivityCommitted{}, ctx.Err()
	case <-w.done:
		return ActivityCommitted{}, errors.New("activity worker is closed")
	}
	select {
	case response := <-reply:
		switch event := response.event.(type) {
		case ActivityCommitted:
			return event, nil
		case ActivityCommitFailed:
			return ActivityCommitted{}, event.Err
		}
	case <-ctx.Done():
		return ActivityCommitted{}, ctx.Err()
	}
	return ActivityCommitted{}, errors.New("invalid activity worker response")
}

func (w *Worker) Clear(ctx context.Context, request ActivityClearRequested) (ActivityCleared, error) {
	if err := w.admitInstalled(ctx); err != nil {
		return ActivityCleared{}, err
	}
	defer w.releaseInstalled()
	if w.installed != nil {
		if err := ctx.Err(); err != nil {
			return ActivityCleared{}, err
		}
	}
	reply := make(chan workerReply, 1)
	select {
	case w.requests <- workerRequest{clear: &request, reply: reply}:
		w.ackInstalled(request.Sequence)
	case <-ctx.Done():
		return ActivityCleared{}, ctx.Err()
	case <-w.done:
		return ActivityCleared{}, errors.New("activity worker is closed")
	}
	select {
	case response := <-reply:
		switch event := response.event.(type) {
		case ActivityCleared:
			return event, nil
		case ActivityClearFailed:
			return ActivityCleared{}, event.Err
		}
	case <-ctx.Done():
		return ActivityCleared{}, ctx.Err()
	}
	return ActivityCleared{}, errors.New("invalid activity worker response")
}

func (w *Worker) Discard(ctx context.Context, request ActivityDiscardRequested) (ActivityDiscarded, error) {
	if err := w.admitInstalled(ctx); err != nil {
		return ActivityDiscarded{}, err
	}
	defer w.releaseInstalled()
	if w.installed != nil {
		if err := ctx.Err(); err != nil {
			return ActivityDiscarded{}, err
		}
	}
	reply := make(chan workerReply, 1)
	select {
	case w.requests <- workerRequest{discard: &request, reply: reply}:
		w.ackInstalled(request.Sequence)
	case <-ctx.Done():
		return ActivityDiscarded{}, ctx.Err()
	case <-w.done:
		return ActivityDiscarded{}, errors.New("activity worker is closed")
	}
	select {
	case response := <-reply:
		switch event := response.event.(type) {
		case ActivityDiscarded:
			return event, nil
		case ActivityDiscardFailed:
			return ActivityDiscarded{}, event.Err
		}
	case <-ctx.Done():
		return ActivityDiscarded{}, ctx.Err()
	}
	return ActivityDiscarded{}, errors.New("invalid activity worker response")
}

func (w *Worker) Close(ctx context.Context) error {
	if w.installed != nil {
		if ctx == nil {
			return ErrInstalledContext
		}
		w.beginInstalledDrain()
		select {
		case <-w.installed.done:
			return w.installed.result
		case <-ctx.Done():
			select {
			case <-w.installed.done:
				return w.installed.result
			default:
				return ctx.Err()
			}
		}
	}
	select {
	case w.requests <- workerRequest{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *Worker) run() {
	defer close(w.done)
	for request := range w.requests {
		if request.commit == nil && request.clear == nil && request.discard == nil {
			return
		}
		if request.commit != nil {
			entry := NormalizeEntry(request.commit.Entry)
			err := w.journal.Append(entry)
			w.retainInstalledError(err)
			if err != nil {
				request.reply <- workerReply{event: ActivityCommitFailed{Sequence: request.commit.Sequence, Err: err}}
			} else {
				request.reply <- workerReply{event: ActivityCommitted{Sequence: request.commit.Sequence, Entry: entry}}
			}
			continue
		}
		if request.discard != nil {
			err := w.journal.Discard(request.discard.ID)
			w.retainInstalledError(err)
			if err != nil {
				request.reply <- workerReply{event: ActivityDiscardFailed{
					Sequence: request.discard.Sequence,
					ID:       request.discard.ID,
					Err:      err,
				}}
			} else {
				request.reply <- workerReply{event: ActivityDiscarded{
					Sequence: request.discard.Sequence,
					ID:       request.discard.ID,
				}}
			}
			continue
		}
		err := w.journal.Clear()
		w.retainInstalledError(err)
		if err != nil {
			request.reply <- workerReply{event: ActivityClearFailed{Sequence: request.clear.Sequence, Err: err}}
		} else {
			request.reply <- workerReply{event: ActivityCleared{Sequence: request.clear.Sequence}}
		}
	}
}
