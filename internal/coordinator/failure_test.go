package coordinator

import (
	"context"
	"errors"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func TestPaneAndTopologyFailuresAgreeOnPhase(t *testing.T) {
	cli := func(code string) error { return &herdr.CLIError{Code: code, Message: "private"} }
	started := func(err error) error { return &herdr.OutcomeError{Started: true, Err: err} }
	tests := []struct {
		name  string
		err   error
		phase string
	}{
		{"refused after start", started(cli("invalid_request")), "not_started"},
		{"transient refusal after start", started(cli("server_not_running")), "not_started"},
		{"operation in progress", started(cli("worktree_operation_in_progress")), "not_started"},
		{"partially applied", partiallyApplied("earlier step", started(cli("invalid_request"))), "dispatched_unknown"},
		{"created target unknown", errors.Join(herdr.ErrDispatchedUnknown, herdr.ErrCreatedTargetUnknown), "dispatched_unknown"},
		{"started", started(errors.New("private")), "dispatched_unknown"},
		{"not started", &herdr.OutcomeError{Err: errors.New("private")}, "not_started"},
		{"deadline", context.DeadlineExceeded, "not_started"},
		{"unclassified", errors.New("private"), "failed"},
	}
	d := NewDispatcher(nil, NewState(testLogger()), nil, testLogger())
	t.Cleanup(func() { _ = d.Close(context.Background()) })
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := d.failErr("r", "send_keys", "pane", tt.err); got.Phase != tt.phase {
				t.Fatalf("pane phase = %q, want %q", got.Phase, tt.phase)
			}
			if got := d.failTopologyErr("r", "workspace_rename", "", tt.err); got.Phase != tt.phase {
				t.Fatalf("topology phase = %q, want %q", got.Phase, tt.phase)
			}
		})
	}
}
