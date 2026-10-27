package coordinator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func TestRetryAgentStartCancellationBoundaries(t *testing.T) {
	for _, boundary := range []string{"backoff", "before dispatch", "after dispatch"} {
		t.Run(boundary, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			refusal := &herdr.OutcomeError{Started: true, Err: &herdr.CLIError{Code: "agent_pane_busy"}}
			attempts := 0
			err := retryAgentStart(ctx, func() error {
				attempts++
				if attempts == 1 {
					return refusal
				}
				if ctx.Err() == nil {
					t.Fatal("next dispatch did not observe cancellation")
				}
				return &herdr.OutcomeError{Started: boundary == "after dispatch", Err: ctx.Err()}
			}, func(waitCtx context.Context, delay time.Duration) bool {
				if delay != agentStartRetryInitial {
					t.Fatalf("delay = %s", delay)
				}
				cancel()
				if boundary == "backoff" {
					return waitForAgentRetry(waitCtx, time.Hour)
				}
				return true
			})
			switch boundary {
			case "backoff":
				if err != refusal || attempts != 1 {
					t.Fatalf("error = %v, attempts = %d", err, attempts)
				}
			case "before dispatch":
				if !errors.Is(err, herdr.ErrNotStarted) || herdr.IsRefused(err) || attempts != 2 {
					t.Fatalf("error = %v, attempts = %d", err, attempts)
				}
			case "after dispatch":
				if !errors.Is(err, herdr.ErrDispatchedUnknown) || herdr.IsRefused(err) || attempts != 2 {
					t.Fatalf("error = %v, attempts = %d", err, attempts)
				}
			}
		})
	}
}
