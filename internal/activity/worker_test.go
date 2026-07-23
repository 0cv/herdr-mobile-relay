package activity

import (
	"context"
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
