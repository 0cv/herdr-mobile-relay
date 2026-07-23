package coordinator

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSchedulerCompletesBurstWithinOwnedCapacity(t *testing.T) {
	const (
		capacity = 4
		total    = 80
	)
	scheduler := NewScheduler(capacity, testLogger())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = scheduler.Close(ctx)
	})

	release := make(chan struct{})
	firstWave := make(chan struct{}, capacity)
	var active atomic.Int32
	var peak atomic.Int32
	var wg sync.WaitGroup
	results := make(chan error, total)
	for index := 0; index < total; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			now := time.Now()
			_, err := scheduler.Execute(context.Background(), ScheduleOptions{Command: Command{
				ID:         scheduler.NextCommandID(),
				RequestID:  fmt.Sprintf("request-%d", index),
				ReceivedAt: now,
				Deadline:   now.Add(5 * time.Second),
				Kind:       CommandPrompt,
				PaneID:     fmt.Sprintf("pane-%d", index),
			}}, EffectFunc(func(context.Context, WorkerToken) EffectResult {
				current := active.Add(1)
				observeInt32Max(&peak, current)
				select {
				case firstWave <- struct{}{}:
				default:
				}
				<-release
				active.Add(-1)
				return EffectResult{Result: completed("", "prompt", "", nil)}
			}))
			results <- err
		}(index)
	}

	for index := 0; index < capacity; index++ {
		select {
		case <-firstWave:
		case <-time.After(time.Second):
			t.Fatal("scheduler did not fill its owned capacity")
		}
	}
	close(release)

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("burst did not complete; protected completions may be starved")
	}
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("execute burst: %v", err)
		}
	}
	if got := peak.Load(); got != capacity {
		t.Fatalf("peak concurrency = %d, want %d", got, capacity)
	}
	metrics := scheduler.Metrics()
	if metrics.Completed != total || metrics.HerdrInUse != 0 {
		t.Fatalf("scheduler metrics after burst = %+v", metrics)
	}
}

func TestSchedulerPreservesTenClientReceiptOrderForOnePane(t *testing.T) {
	const total = 10
	scheduler := NewScheduler(8, testLogger())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = scheduler.Close(ctx)
	})

	var mu sync.Mutex
	order := make([]int, 0, total)
	results := make(chan error, total)
	for index := 0; index < total; index++ {
		admitted := make(chan struct{})
		go func(index int) {
			now := time.Now()
			_, err := scheduler.ExecuteAdmitted(context.Background(), ScheduleOptions{Command: Command{
				ID:         scheduler.NextCommandID(),
				RequestID:  fmt.Sprintf("client-%d", index),
				ReceivedAt: now,
				Deadline:   now.Add(2 * time.Second),
				Kind:       CommandPrompt,
				PaneID:     "pane-1",
			}}, EffectFunc(func(context.Context, WorkerToken) EffectResult {
				mu.Lock()
				order = append(order, index)
				mu.Unlock()
				return EffectResult{Result: completed("", "prompt", "pane-1", nil)}
			}), func() { close(admitted) })
			results <- err
		}(index)
		<-admitted
	}
	for index := 0; index < total; index++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	for index, got := range order {
		if got != index {
			t.Fatalf("effect order = %v, want receipt order 0..%d", order, total-1)
		}
	}
}

func TestSchedulerShutdownCancelsPendingWork(t *testing.T) {
	scheduler := NewScheduler(1, testLogger())
	started := make(chan struct{})
	cancelled := make(chan struct{})
	go func() {
		now := time.Now()
		_, _ = scheduler.Execute(context.Background(), ScheduleOptions{Command: Command{
			ID:         scheduler.NextCommandID(),
			RequestID:  "pending",
			ReceivedAt: now,
			Deadline:   now.Add(30 * time.Second),
			Kind:       CommandPrompt,
			PaneID:     "pane-1",
		}}, EffectFunc(func(ctx context.Context, _ WorkerToken) EffectResult {
			close(started)
			<-ctx.Done()
			close(cancelled)
			return EffectResult{Result: &CommandResult{
				Action: "prompt", Phase: "dispatched_unknown", Error: "cancelled",
			}}
		}))
	}()
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := scheduler.Close(ctx); err != nil {
		t.Fatalf("close scheduler with pending work: %v", err)
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("pending worker was not cancelled during shutdown")
	}
}

func TestApprovalExpiresNotStartedWhileQueued(t *testing.T) {
	scheduler := NewScheduler(1, testLogger())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = scheduler.Close(ctx)
	})
	blockingStarted := make(chan struct{})
	release := make(chan struct{})
	go func() {
		now := time.Now()
		_, _ = scheduler.Execute(context.Background(), ScheduleOptions{Command: Command{
			ID:         scheduler.NextCommandID(),
			RequestID:  "slow",
			ReceivedAt: now,
			Deadline:   now.Add(time.Second),
			Kind:       CommandPrompt,
			PaneID:     "pane-1",
		}}, EffectFunc(func(context.Context, WorkerToken) EffectResult {
			close(blockingStarted)
			<-release
			return EffectResult{Result: completed("slow", "prompt", "pane-1", nil)}
		}))
	}()
	<-blockingStarted

	var ran atomic.Bool
	now := time.Now()
	result, err := scheduler.Execute(context.Background(), ScheduleOptions{
		Command: Command{
			ID:         scheduler.NextCommandID(),
			RequestID:  "approval",
			ReceivedAt: now,
			Deadline:   now.Add(100 * time.Millisecond),
			Kind:       CommandApproval,
			PaneID:     "pane-1",
		},
		LedgerKey:   "approval\x00pane-1\x00event-1",
		PayloadHash: "choice-1",
	}, EffectFunc(func(context.Context, WorkerToken) EffectResult {
		ran.Store(true)
		return EffectResult{Result: completed("approval", "approval", "pane-1", nil)}
	}))
	close(release)

	if err != nil || result == nil || result.Phase != "not_started" {
		t.Fatalf("queued approval result = %+v, err = %v", result, err)
	}
	if ran.Load() {
		t.Fatal("expired queued approval effect ran")
	}
}

func TestConfirmationWatchCannotLandAfterGenerationBump(t *testing.T) {
	scheduler := NewScheduler(1, testLogger())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = scheduler.Close(ctx)
	})
	const approvalKey = "approval\x00pane-1\x00event-1"
	execute := func(kind CommandKind, key string, bump bool) {
		t.Helper()
		now := time.Now()
		result, err := scheduler.Execute(context.Background(), ScheduleOptions{
			Command: Command{
				ID:         scheduler.NextCommandID(),
				RequestID:  string(kind),
				ReceivedAt: now,
				Deadline:   now.Add(time.Second),
				Kind:       kind,
				PaneID:     "pane-1",
			},
			LedgerKey:   key,
			PayloadHash: string(kind),
		}, EffectFunc(func(context.Context, WorkerToken) EffectResult {
			return EffectResult{
				Result:         completed(string(kind), string(kind), "pane-1", nil),
				BumpGeneration: bump,
			}
		}))
		if err != nil || result == nil || !result.OK {
			t.Fatalf("%s result = %+v, err = %v", kind, result, err)
		}
	}

	execute(CommandApproval, approvalKey, false)
	execute(CommandClear, "clear\x00pane-1", true)
	if scheduler.UpdateLedgerPhase(approvalKey, 0, "confirmed") {
		t.Fatal("stale confirmation watch updated a ledger after generation bump")
	}
}

func observeInt32Max(target *atomic.Int32, value int32) {
	for {
		current := target.Load()
		if value <= current || target.CompareAndSwap(current, value) {
			return
		}
	}
}
