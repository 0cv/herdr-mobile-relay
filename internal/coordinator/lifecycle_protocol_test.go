package coordinator

import (
	"context"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func TestStartKindAgentDoesNotRetryProtocolMismatch(t *testing.T) {
	client := newFakeBackend()
	client.setHook("StartAgent", func(context.Context, fakeCall) error {
		return &herdr.OutcomeError{Started: true, Err: &herdr.CLIError{Code: "protocol_mismatch", Message: "incompatible"}}
	})
	lifecycle := &Lifecycle{herdr: client}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	err := lifecycle.startKindAgent(ctx, "codex", "project", "pane-1", nil)
	if err == nil || !herdr.IsRefused(err) || herdr.IsTransientRefused(err) {
		t.Fatalf("error = %v, want permanent refusal without transient retry", err)
	}
	if attempts := client.count("StartAgent"); attempts != 1 {
		t.Fatalf("attempts = %d, want one protocol-refused dispatch", attempts)
	}
}
