package activity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWorkerOrdersCommitDiscardClear(t *testing.T) {
	journal, err := OpenJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	worker := NewWorker(journal)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := worker.Close(ctx); err != nil {
			t.Errorf("close worker: %v", err)
		}
	})

	first := NewEntry("blocked", "attention", "first", "p1", "", "", "")
	second := NewEntry("finished", "completed", "second", "p1", "", "", "")
	if _, err := worker.Commit(context.Background(), ActivityCommitRequested{Sequence: 1, Entry: first}); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.Commit(context.Background(), ActivityCommitRequested{Sequence: 2, Entry: second}); err != nil {
		t.Fatal(err)
	}
	discarded, err := worker.Discard(context.Background(), ActivityDiscardRequested{Sequence: 3, ID: first.ID})
	if err != nil {
		t.Fatal(err)
	}
	if discarded.Sequence != 3 || discarded.ID != first.ID {
		t.Fatalf("discarded = %+v", discarded)
	}
	if recent := journal.Recent(10); len(recent) != 1 || recent[0].ID != second.ID {
		t.Fatalf("after discard recent = %+v", recent)
	}
	if _, err := worker.Clear(context.Background(), ActivityClearRequested{Sequence: 4}); err != nil {
		t.Fatal(err)
	}
	if recent := journal.Recent(10); len(recent) != 0 {
		t.Fatalf("after clear recent = %+v", recent)
	}
}

func newWorkerTest(t *testing.T) (*Worker, *Journal, context.Context) {
	t.Helper()
	journal, err := OpenJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	worker := NewWorker(journal)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(func() {
		cancel()
		select {
		case <-worker.done:
			return
		default:
		}
		closeCtx, closeCancel := context.WithTimeout(context.Background(), time.Second)
		defer closeCancel()
		if err := worker.Close(closeCtx); err != nil {
			t.Errorf("close worker: %v", err)
		}
	})
	return worker, journal, ctx
}

func gateWorkerJournal(t *testing.T, journal *Journal) func() {
	t.Helper()
	journal.mu.RLock()
	var once sync.Once
	release := func() { once.Do(journal.mu.RUnlock) }
	t.Cleanup(release)
	return release
}

func startWorkerTestCall(t *testing.T, parent context.Context, call func(context.Context) error) <-chan error {
	t.Helper()
	ctx, cancel := context.WithCancel(parent)
	result := make(chan error, 1)
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		result <- call(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-joined:
		case <-time.After(time.Second):
			t.Error("worker caller did not exit after cancellation")
		}
	})
	return result
}

func awaitWorkerTestCall(t *testing.T, ctx context.Context, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		t.Fatalf("waiting for worker caller: %v", ctx.Err())
		return ctx.Err()
	}
}

func awaitWorkerQueue(t *testing.T, ctx context.Context, worker *Worker, count int) {
	t.Helper()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		// The held read lock lets us observe a pending journal writer without
		// requiring the scheduler to expose the brief dequeue transition.
		writerWaiting := !worker.journal.mu.TryRLock()
		if !writerWaiting {
			worker.journal.mu.RUnlock()
		}
		if writerWaiting && len(worker.requests) == count {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("queue length = %d, want %d with a pending writer: %v", len(worker.requests), count, ctx.Err())
		case <-ticker.C:
		}
	}
}

type workerTestOperation struct {
	name string
	call func(context.Context, uint64) (any, error)
	zero any
}

func workerTestOperations(worker *Worker, entry Entry) []workerTestOperation {
	return []workerTestOperation{
		{"commit", func(ctx context.Context, sequence uint64) (any, error) {
			return worker.Commit(ctx, ActivityCommitRequested{Sequence: sequence, Entry: entry})
		}, ActivityCommitted{}},
		{"clear", func(ctx context.Context, sequence uint64) (any, error) {
			return worker.Clear(ctx, ActivityClearRequested{Sequence: sequence})
		}, ActivityCleared{}},
		{"discard", func(ctx context.Context, sequence uint64) (any, error) {
			return worker.Discard(ctx, ActivityDiscardRequested{Sequence: sequence, ID: entry.ID})
		}, ActivityDiscarded{}},
	}
}

// With the journal locked, 65 admitted calls mean one is executing and all 64
// queue slots are occupied. No worker operation or private reply is fabricated.
func fillWorkerQueue(t *testing.T, ctx context.Context, worker *Worker) []<-chan error {
	t.Helper()
	if cap(worker.requests) != 64 {
		t.Fatalf("queue capacity = %d, want 64", cap(worker.requests))
	}
	var results []<-chan error
	for i := 0; i < 65; i++ {
		entry := Entry{ID: fmt.Sprintf("queued-%d", i), Summary: "queued"}
		operation := workerTestOperations(worker, entry)[0]
		results = append(results, startWorkerTestCall(t, ctx, func(ctx context.Context) error {
			result, err := operation.call(ctx, uint64(i+1))
			if err != nil && !reflect.DeepEqual(result, operation.zero) {
				return fmt.Errorf("commit error returned nonzero result: %+v", result)
			}
			return err
		}))
	}
	awaitWorkerQueue(t, ctx, worker, 64)
	return results
}

func TestWorkerSuccessEchoesAndPersistsNormalizedEntry(t *testing.T) {
	worker, journal, ctx := newWorkerTest(t)
	entry := NewEntry("prompt", "sent", "normalized", "pane", "agent", "project", "request")
	entry.Extract = strings.Repeat("界", maxExtractChars+7)
	entry.Details = map[string]any{"action": "prompt"}
	want := entry
	want.Extract = strings.Repeat("界", maxExtractChars)
	committed, err := worker.Commit(ctx, ActivityCommitRequested{Sequence: 91, Entry: entry})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(committed, ActivityCommitted{Sequence: 91, Entry: want}) {
		t.Fatal("commit did not echo sequence and normalized entry")
	}
	reopened, err := OpenJournal(journal.dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Recent(0); !reflect.DeepEqual(got, []Entry{want}) {
		t.Fatal("returned entry differs from persisted normalized entry")
	}
	discarded, err := worker.Discard(ctx, ActivityDiscardRequested{Sequence: 92, ID: entry.ID})
	if err != nil || discarded != (ActivityDiscarded{Sequence: 92, ID: entry.ID}) {
		t.Fatalf("discard = %+v, %v", discarded, err)
	}
	cleared, err := worker.Clear(ctx, ActivityClearRequested{Sequence: 93})
	if err != nil || cleared != (ActivityCleared{Sequence: 93}) {
		t.Fatalf("clear = %+v, %v", cleared, err)
	}
}

func TestWorkerJournalErrorsReturnZeroSuccess(t *testing.T) {
	for _, name := range []string{"commit", "clear", "discard"} {
		t.Run(name, func(t *testing.T) {
			worker, journal, ctx := newWorkerTest(t)
			entry := Entry{ID: "seed", Summary: "seed"}
			if _, err := worker.Commit(ctx, ActivityCommitRequested{Sequence: 1, Entry: entry}); err != nil {
				t.Fatal(err)
			}
			// Real filesystem failures inside Journal, after successful setup.
			journal.dir = filepath.Join(journal.dir, "missing")
			journal.path = filepath.Join(journal.dir, "activity.jsonl")
			prefix := map[string]string{
				"commit":  "open activity journal: ",
				"clear":   "prepare activity clear: ",
				"discard": "prepare stale activity discard: ",
			}[name]
			for _, operation := range workerTestOperations(worker, entry) {
				if operation.name != name {
					continue
				}
				result, err := operation.call(ctx, 99)
				var pathErr *os.PathError
				if err == nil || !errors.Is(err, os.ErrNotExist) || !errors.As(err, &pathErr) || !strings.HasPrefix(err.Error(), prefix) {
					t.Fatalf("%s error = %v, want wrapped missing-path cause with prefix %q", name, err, prefix)
				}
				if !reflect.DeepEqual(result, operation.zero) {
					t.Fatalf("%s error returned nonzero success: %+v", name, result)
				}
			}
		})
	}
}

func TestWorkerSaturatedAdmissionCancellation(t *testing.T) {
	worker, journal, ctx := newWorkerTest(t)
	release := gateWorkerJournal(t, journal)
	results := fillWorkerQueue(t, ctx, worker)
	for _, operation := range workerTestOperations(worker, Entry{ID: "not-admitted"}) {
		t.Run(operation.name, func(t *testing.T) {
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			// Only cancellation is ready: the queue cannot drain under the gate.
			result, err := operation.call(canceled, 100)
			if err != context.Canceled || !reflect.DeepEqual(result, operation.zero) {
				t.Fatalf("saturated call = %+v, %v", result, err)
			}
			if len(worker.requests) != 64 {
				t.Fatal("canceled admission changed the full queue")
			}
		})
	}
	release()
	for _, result := range results {
		if err := awaitWorkerTestCall(t, ctx, result); err != nil {
			t.Fatal(err)
		}
	}
	if got := journal.Recent(0); len(got) != 65 {
		t.Fatalf("persisted entries = %d, want only 65 admitted commits", len(got))
	}
}

func TestWorkerAcceptedCancellationStillPersistsAndRepliesDoNotStrand(t *testing.T) {
	for _, name := range []string{"commit", "clear", "discard"} {
		t.Run(name, func(t *testing.T) {
			worker, journal, ctx := newWorkerTest(t)
			entry := Entry{ID: "target"}
			if _, err := worker.Commit(ctx, ActivityCommitRequested{Entry: entry}); err != nil {
				t.Fatal(err)
			}
			release := gateWorkerJournal(t, journal)
			blocker := startWorkerTestCall(t, ctx, func(ctx context.Context) error {
				_, err := worker.Commit(ctx, ActivityCommitRequested{Entry: Entry{ID: "blocker"}})
				return err
			})
			awaitWorkerQueue(t, ctx, worker, 0)
			callCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			for _, operation := range workerTestOperations(worker, entry) {
				if operation.name != name {
					continue
				}
				result := startWorkerTestCall(t, callCtx, func(ctx context.Context) error {
					value, err := operation.call(ctx, 9)
					if !reflect.DeepEqual(value, operation.zero) {
						return fmt.Errorf("canceled call returned nonzero success: %+v", value)
					}
					return err
				})
				awaitWorkerQueue(t, ctx, worker, 1)
				cancel()
				if err := awaitWorkerTestCall(t, ctx, result); err != context.Canceled {
					t.Fatalf("accepted caller error = %v, want context.Canceled", err)
				}
			}
			if len(worker.requests) != 1 {
				t.Fatal("caller cancellation removed accepted work")
			}
			release()
			if err := awaitWorkerTestCall(t, ctx, blocker); err != nil {
				t.Fatal(err)
			}
			// An abandoned reply must not prevent the next journal operation.
			if _, err := worker.Commit(ctx, ActivityCommitRequested{Entry: Entry{ID: "next"}}); err != nil {
				t.Fatalf("next commit stranded: %v", err)
			}
			reopened, err := OpenJournal(journal.dir)
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, entry := range reopened.Recent(0) {
				ids = append(ids, entry.ID)
			}
			want := map[string][]string{
				"commit":  {"target", "blocker", "target", "next"},
				"clear":   {"next"},
				"discard": {"blocker", "next"},
			}[name]
			if !reflect.DeepEqual(ids, want) {
				t.Fatalf("persisted IDs after canceled %s = %v, want %v", name, ids, want)
			}
		})
	}
}

func TestWorkerOrdinaryCloseWaitsForAcceptedWork(t *testing.T) {
	worker, journal, ctx := newWorkerTest(t)
	release := gateWorkerJournal(t, journal)
	results := fillWorkerQueue(t, ctx, worker)
	closed := startWorkerTestCall(t, ctx, worker.Close)
	release()
	if err := awaitWorkerTestCall(t, ctx, closed); err != nil {
		t.Fatal(err)
	}
	for _, result := range results {
		if err := awaitWorkerTestCall(t, ctx, result); err != nil {
			t.Fatal(err)
		}
	}
	reopened, err := OpenJournal(journal.dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Recent(0); len(got) != 65 {
		t.Fatalf("close returned before accepted commits persisted: %d entries", len(got))
	}
}
