package coordinator

import (
	"context"
	"maps"
	"sync"
	"testing"
	"time"
)

func TestScrollReservationReleasedWhenQueuedIntentExpires(t *testing.T) {
	f := newScrollFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	blocked := make(chan *CommandResult, 1)
	go func() {
		blocked <- f.d.schedule(context.Background(), ScheduleOptions{Command: f.d.command(context.Background(), time.Now(), "blocking-input", CommandInput, "pane", time.Second, nil)}, EffectFunc(func(context.Context, WorkerToken) EffectResult {
			close(entered)
			<-release
			return EffectResult{Result: completed("blocking-input", "send_input", "pane", nil)}
		}))
	}()
	<-entered
	message := f.message("scroll_agent", "queued-expiry", "owner")
	message["_server_received_at"] = time.Now().Add(-450 * time.Millisecond).Format(time.RFC3339Nano)
	result := f.d.Handle(context.Background(), message)
	if result.OK || result.Phase != "not_started" || f.count("pane.send_text") != 0 {
		t.Fatalf("expired queued intent: %+v", result)
	}
	releaseSlot, err := f.d.scroll.Begin("pane")
	if err != nil {
		t.Fatalf("queued expiry leaked reservation: %v", err)
	}
	releaseSlot()
	release <- struct{}{}
	if result := <-blocked; !result.OK {
		t.Fatalf("blocking input: %+v", result)
	}
}

func TestScrollInFlightDuplicateAttachesBeforeBusyAdmission(t *testing.T) {
	for _, outcome := range []string{"completed", "dispatched_unknown"} {
		t.Run(outcome, func(t *testing.T) {
			f := newScrollFixture(t)
			entered, release := make(chan struct{}), make(chan struct{})
			var unblock sync.Once
			defer unblock.Do(func() { close(release) })
			f.mu.Lock()
			f.rawHook = func() { close(entered); <-release }
			if outcome == "dispatched_unknown" {
				f.rawMode = "drop"
			}
			f.mu.Unlock()
			message := f.message("scroll_agent", "same-request", "owner")
			first, duplicate := make(chan *CommandResult, 1), make(chan *CommandResult, 1)
			go func() { first <- f.d.Handle(context.Background(), message) }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("first wheel never reached write")
			}
			admitted := make(chan struct{})
			go func() {
				duplicate <- f.d.HandleAdmitted(context.Background(), maps.Clone(message), func() { close(admitted) })
			}()
			select {
			case <-admitted:
			case <-time.After(time.Second):
				t.Fatal("duplicate was not admitted")
			}
			select {
			case result := <-duplicate:
				t.Fatalf("in-flight duplicate was rejected instead of attached: %+v", result)
			case <-time.After(20 * time.Millisecond):
			}
			conflict := maps.Clone(message)
			conflict["direction"] = "down"
			if result := f.d.Handle(context.Background(), conflict); result.OK || f.count("pane.send_text") != 1 {
				t.Fatalf("conflicting duplicate: %+v", result)
			}
			different := maps.Clone(message)
			different["request_id"] = "different-request"
			start := time.Now()
			busy := f.d.Handle(context.Background(), different)
			if busy.OK || busy.Phase != "not_started" || time.Since(start) > 50*time.Millisecond {
				t.Fatalf("different request backlogged: %+v", busy)
			}
			unblock.Do(func() { close(release) })
			var original, replayed *CommandResult
			select {
			case original = <-first:
			case <-time.After(time.Second):
				t.Fatal("original did not finish")
			}
			select {
			case replayed = <-duplicate:
			case <-time.After(time.Second):
				t.Fatal("attached duplicate did not finish")
			}
			if original.Phase != outcome || replayed.Phase != outcome || original.OK != replayed.OK || !replayed.replayed || f.count("pane.send_text") != 1 || f.count("pane.process_info") != 2 || f.bridgeCalls.Load() != 1 || f.geometryCalls.Load() != 1 {
				t.Fatalf("duplicate changed effect/outcome/budget: %+v %+v", original, replayed)
			}
			releaseSlot, err := f.d.scroll.Begin("pane")
			if err != nil {
				t.Fatalf("reservation leaked: %v", err)
			}
			releaseSlot()
		})
	}
}
