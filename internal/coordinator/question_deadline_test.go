package coordinator

import (
	"context"
	"errors"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func TestQuestionDeadlineDuringInterKeyDelayIsDispatchedUnknown(t *testing.T) {
	client := newFakeBackend()
	dispatcher := newTestDispatcher(t, client, NewState(testLogger()))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.setHook("SendKeys", func(context.Context, fakeCall) error {
		cancel()
		return nil
	})

	err := dispatcher.sendQuestionKeys(ctx, "pane-1", []string{"Down", "Enter"})
	if !errors.Is(err, herdr.ErrDispatchedUnknown) {
		t.Fatalf("partial question input = %v, want dispatched_unknown", err)
	}
	if errors.Is(err, herdr.ErrNotStarted) {
		t.Fatalf("partial question input was also classified not_started: %v", err)
	}
	if sends := client.callsTo("SendKeys"); len(sends) != 1 {
		t.Fatalf("send count = %d, want exactly the first key before expiry: %+v", len(sends), client.recorded())
	}
}
