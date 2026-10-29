package activity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestInstalledActivityRetainsCanceledReplyFailure(t *testing.T) {
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
	observed := make(chan struct{})
	accepted := make(chan uint64, 3)
	w.installed.accepted = func(id uint64) { accepted <- id }
	defer func() { cancel(); unlock(); callers.Wait(); <-observed; _ = w.DrainInstalled() }()
	go func() { <-ctx.Done(); callers.Wait(); close(observed) }()
	for i := 0; i < 2; i++ {
		callers.Add(1)
		go func(i int) {
			defer callers.Done()
			_, _ = w.Commit(ctx, ActivityCommitRequested{Sequence: uint64(i), Entry: Entry{ID: fmt.Sprint(i), Details: map[string]any{"unsupported": make(chan int)}}})
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
	// At least one accepted operation will fail, despite cancellation winning
	// every reply select while the real journal lock is still held.
	w.beginInstalledDrain()
	select {
	case <-w.installed.done:
		t.Fatal("drained with accepted journal work held")
	default:
	}
	unlock()
	var encodingError *json.UnsupportedTypeError
	if err := w.DrainInstalled(); !errors.As(err, &encodingError) {
		t.Fatalf("lost journal failure: %v", err)
	}
}
