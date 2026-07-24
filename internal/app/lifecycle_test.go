package app

import (
	"context"
	"testing"
	"time"
)

func TestLifecycleTasksStopCancelsDrainsAndClosesAdmission(t *testing.T) {
	tasks := newLifecycleTasks(context.Background())
	started := make(chan struct{})
	finished := make(chan struct{})
	if !tasks.Start(func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		close(finished)
	}) {
		t.Fatal("initial task was rejected")
	}
	<-started

	tasks.Stop()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("Stop returned before task observed cancellation")
	}
	if tasks.Start(func(context.Context) {}) {
		t.Fatal("task admitted after Stop")
	}
}
