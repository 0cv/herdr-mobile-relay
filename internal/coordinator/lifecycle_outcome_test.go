package coordinator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/profiles"
)

func TestCustomAgentRenameRefusalIsPartiallyApplied(t *testing.T) {
	client := newFakeBackend()
	client.agent = herdr.AgentInfo{PaneID: "pane-1", Running: true}
	client.setHook("RenameAgent", func(context.Context, fakeCall) error {
		return &herdr.OutcomeError{Started: true, Err: &herdr.CLIError{Code: "pane_not_found"}}
	})
	lifecycle := &Lifecycle{herdr: client}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	err := lifecycle.startInTarget(ctx, profiles.Profile{Argv: []string{"custom-agent"}}, StartRequest{Name: "agent"}, "pane-1")
	if !errors.Is(err, herdr.ErrPartiallyApplied) {
		t.Fatalf("rename failure = %v, want partially applied", err)
	}
	result := (&Dispatcher{logger: testLogger()}).failErr("start", "agent_start", "pane-1", err)
	if result.Phase != "dispatched_unknown" {
		t.Fatalf("result = %+v, want uncertain dispatch", result)
	}
}

func TestCustomAgentTimeoutAfterPaneRunIsDispatchedUnknown(t *testing.T) {
	client := newFakeBackend()
	lifecycle := &Lifecycle{herdr: client}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := lifecycle.startInTarget(ctx, profiles.Profile{Argv: []string{"custom-agent"}}, StartRequest{Name: "agent"}, "pane-1")
	if !errors.Is(err, herdr.ErrDispatchedUnknown) {
		t.Fatalf("custom agent timeout = %v, want dispatched_unknown", err)
	}
	if errors.Is(err, herdr.ErrNotStarted) {
		t.Fatalf("custom agent timeout was also classified not_started: %v", err)
	}
	if runs := client.count("PaneRun"); runs != 1 {
		t.Fatalf("PaneRun calls = %d, want 1", runs)
	}
}
