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
	entry Entry
	err   error
}

type Worker struct {
	journal  *Journal
	requests chan workerRequest
	done     chan struct{}
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
	response, err := w.submit(ctx, workerRequest{commit: &request})
	if err != nil {
		return ActivityCommitted{}, err
	}
	return ActivityCommitted{Sequence: request.Sequence, Entry: response.entry}, nil
}

func (w *Worker) Clear(ctx context.Context, request ActivityClearRequested) (ActivityCleared, error) {
	if _, err := w.submit(ctx, workerRequest{clear: &request}); err != nil {
		return ActivityCleared{}, err
	}
	return ActivityCleared{Sequence: request.Sequence}, nil
}

func (w *Worker) Discard(ctx context.Context, request ActivityDiscardRequested) (ActivityDiscarded, error) {
	if _, err := w.submit(ctx, workerRequest{discard: &request}); err != nil {
		return ActivityDiscarded{}, err
	}
	return ActivityDiscarded{Sequence: request.Sequence, ID: request.ID}, nil
}

func (w *Worker) submit(ctx context.Context, request workerRequest) (workerReply, error) {
	reply := make(chan workerReply, 1)
	request.reply = reply
	select {
	case w.requests <- request:
	case <-ctx.Done():
		return workerReply{}, ctx.Err()
	case <-w.done:
		return workerReply{}, errors.New("activity worker is closed")
	}
	select {
	case response := <-reply:
		return response, response.err
	case <-ctx.Done():
		return workerReply{}, ctx.Err()
	}
}

func (w *Worker) Close(ctx context.Context) error {
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
			request.reply <- workerReply{entry: entry, err: w.journal.Append(entry)}
			continue
		}
		if request.discard != nil {
			request.reply <- workerReply{err: w.journal.Discard(request.discard.ID)}
			continue
		}
		request.reply <- workerReply{err: w.journal.Clear()}
	}
}
