package coordinator

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/activity"
)

func newClearActivitiesDispatcher(t *testing.T) (*Dispatcher, string) {
	t.Helper()
	cache := filepath.Join(t.TempDir(), "cache")
	journal, err := activity.OpenJournal(cache)
	if err != nil {
		t.Fatal(err)
	}
	d := NewDispatcher(nil, NewState(testLogger()), journal, testLogger())
	t.Cleanup(func() { _ = d.Close(context.Background()) })
	return d, cache
}

func TestClearActivitiesFailureReturnsResultWithoutHanging(t *testing.T) {
	d, cache := newClearActivitiesDispatcher(t)
	if err := os.RemoveAll(cache); err != nil {
		t.Fatal(err)
	}
	var delivered []*CommandResult
	done := make(chan *CommandResult, 1)
	go func() {
		done <- d.HandleClearActivities("clear-1", func(result *CommandResult) {
			delivered = append(delivered, result)
		})
	}()

	var result *CommandResult
	select {
	case result = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("HandleClearActivities hung after a failed clear")
	}
	want := CommandResult{
		RequestID: "clear-1", Action: "clear_activities", OK: false, Phase: "failed",
		Error: "Activity history could not be cleared",
	}
	if *result != want {
		t.Fatalf("result = %+v, want %+v", *result, want)
	}
	if len(delivered) != 1 || delivered[0] != result {
		t.Fatalf("delivered = %+v, want the returned result once", delivered)
	}
	if got := d.activities.failures.Load(); got != 2 {
		t.Fatalf("journal failures = %d, want 2 (clear, then the failure activity append)", got)
	}
}

func TestClearActivitiesResultFollowsHistoryBroadcastUnderOrderLock(t *testing.T) {
	d, _ := newClearActivitiesDispatcher(t)
	var mu sync.Mutex
	var events []string
	log := func(event string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, event)
	}
	d.SetBroadcast(func(message any) {
		log(message.(map[string]any)["type"].(string))
	})

	result := d.HandleClearActivities("clear-1", func(result *CommandResult) {
		if d.activities.order.TryLock() {
			d.activities.order.Unlock()
			t.Error("result delivered after the activity ordering lock was released")
		}
		log("result")
	})
	if !result.OK || result.Phase != "completed" {
		t.Fatalf("result = %+v", result)
	}
	d.activities.record("prompt", "sent", "after clear", "", "")

	mu.Lock()
	defer mu.Unlock()
	want := []string{"activity_history", "result", "activity"}
	if len(events) != len(want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("events = %v, want %v", events, want)
		}
	}
}
