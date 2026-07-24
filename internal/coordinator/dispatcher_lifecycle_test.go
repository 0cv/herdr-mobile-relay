package coordinator

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestDispatcherCloseCancelsDrainsAndClosesWatcherAdmission(t *testing.T) {
	dispatcher := NewDispatcher(
		nil,
		NewState(slog.New(slog.NewTextHandler(io.Discard, nil))),
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	started := make(chan struct{})
	finished := make(chan struct{})
	if !dispatcher.startWatcher(func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		close(finished)
	}) {
		t.Fatal("initial watcher was rejected")
	}
	<-started

	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := dispatcher.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	default:
		t.Fatal("Close returned before watcher observed cancellation")
	}
	if dispatcher.startWatcher(func(context.Context) {}) {
		t.Fatal("watcher admitted after Close")
	}
}
