package activity

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestInstalledActivityContextRefusal(t *testing.T) {
	j, err := OpenJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := NewInstalledWorker(j)
	defer w.DrainInstalled()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, c := range []context.Context{nil, ctx} {
		if _, err := w.Commit(c, ActivityCommitRequested{}); err == nil {
			t.Fatal("commit admitted")
		}
		if _, err := w.Clear(c, ActivityClearRequested{}); err == nil {
			t.Fatal("clear admitted")
		}
		if _, err := w.Discard(c, ActivityDiscardRequested{}); err == nil {
			t.Fatal("discard admitted")
		}
	}
	if err := w.Close(nil); !errors.Is(err, ErrInstalledContext) {
		t.Fatal(err)
	}
	if len(w.requests) != 0 {
		t.Fatal("refused request enqueued")
	}
	if _, err := w.Commit(context.Background(), ActivityCommitRequested{Entry: Entry{ID: "kept"}}); err != nil {
		t.Fatal(err)
	}
}
func TestInstalledActivityCompletedErrorConcurrentCloseDrain(t *testing.T) {
	j, err := OpenJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := NewInstalledWorker(j)
	defer w.DrainInstalled()
	_, failure := w.Commit(context.Background(), ActivityCommitRequested{Entry: Entry{Details: map[string]any{"bad": make(chan int)}}})
	first := w.DrainInstalled()
	if failure == nil || !errors.Is(first, failure) {
		t.Fatal("lost accepted error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var calls sync.WaitGroup
	results := make(chan error, 16)
	for i := 0; i < 8; i++ {
		calls.Add(2)
		go func() { defer calls.Done(); results <- w.Close(ctx) }()
		go func() { defer calls.Done(); results <- w.DrainInstalled() }()
	}
	calls.Wait()
	close(results)
	for result := range results {
		if result != first {
			t.Fatal("cached result laundered")
		}
	}
}
func TestInstalledActivityClearDiscardCoverage(t *testing.T) {
	j, err := OpenJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := NewInstalledWorker(j)
	defer w.DrainInstalled()
	for _, id := range []string{"a", "b"} {
		if _, err := w.Commit(context.Background(), ActivityCommitRequested{Entry: Entry{ID: id}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Discard(context.Background(), ActivityDiscardRequested{ID: "a"}); err != nil {
		t.Fatal(err)
	}
	j.mu.RLock()
	entries := append([]Entry(nil), j.entries...)
	j.mu.RUnlock()
	if len(entries) != 1 || entries[0].ID != "b" {
		t.Fatal("discard")
	}
	if _, err := w.Clear(context.Background(), ActivityClearRequested{}); err != nil {
		t.Fatal(err)
	}
	if err := w.DrainInstalled(); err != nil {
		t.Fatal(err)
	}
	if len(j.entries) != 0 {
		t.Fatal("clear")
	}
}
