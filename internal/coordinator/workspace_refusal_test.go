package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func TestTopologyOutcomePrecedence(t *testing.T) {
	cli := func(code, message string) error { return &herdr.CLIError{Code: code, Message: message} }
	started := func(err error) error { return &herdr.OutcomeError{Started: true, Err: err} }
	code := func(c string) any { return map[string]any{"code": c} }
	unknown := map[string]any{"dispatched_unknown": true}
	const uncertain = "Herdr may have completed this command; refresh before retrying"
	tests := []struct {
		name, action   string
		err            error
		phase, message string
		data           any
	}{
		{"dirty force", "worktree_remove", started(cli("dirty_worktree_requires_force", "private")), "not_started", "Worktree has uncommitted changes; force removal is required", map[string]any{"code": "dirty_worktree_requires_force", "force_available": true}},
		{"known beats started", "workspace_reorder", started(cli("invalid_request", " refused ")), "not_started", "refused", code("invalid_request")},
		{"blank refusal", "workspace_reorder", cli("invalid_request", " "), "not_started", "Herdr refused the command", code("invalid_request")},
		{"unknown started", "workspace_reorder", started(errors.New("private")), "dispatched_unknown", uncertain, unknown},
		{"created beats deadline", "workspace_create", errors.Join(context.DeadlineExceeded, herdr.ErrCreatedTargetUnknown), "dispatched_unknown", uncertain, unknown},
		{"started deadline", "workspace_reorder", started(context.DeadlineExceeded), "dispatched_unknown", uncertain, unknown},
		{"not started", "workspace_reorder", &herdr.OutcomeError{Err: errors.New("private")}, "not_started", "Command was not sent; retry is safe", nil},
		{"deadline", "workspace_reorder", context.DeadlineExceeded, "not_started", "Command was not sent; retry is safe", nil},
		{"unknown CLI", "workspace_reorder", cli("future_code", "private"), "failed", "Command failed", nil},
		{"unknown started CLI", "workspace_reorder", started(cli("future_code", "private")), "dispatched_unknown", uncertain, unknown},
		{"list beats unknown", "worktree_list", started(errors.New("private")), "failed", "Worktrees could not be listed", nil},
		{"list beats deadline", "worktree_list", context.DeadlineExceeded, "failed", "Worktrees could not be listed", nil},
		{"list CLI", "worktree_list", started(cli("future_code", " detail ")), "failed", "detail", code("future_code")},
		{"list blank CLI", "worktree_list", cli("future_code", " "), "failed", "Worktrees could not be listed", nil},
		{"refusal beats list", "worktree_list", started(cli("invalid_request", "refused")), "not_started", "refused", code("invalid_request")},
	}
	for _, c := range []string{"protocol_mismatch", "workspace_group_close_required", "workspace_group_changed", "workspace_group_primary_required", "workspace_group_consent_invalid", "workspace_group_validation_unavailable"} {
		tests = append(tests, struct {
			name, action   string
			err            error
			phase, message string
			data           any
		}{c, "workspace_close", started(cli(c, "private")), "not_started", herdr.RefusalMessage(c), code(c)})
	}
	d := NewDispatcher(nil, NewState(testLogger()), nil, testLogger())
	t.Cleanup(func() { _ = d.Close(context.Background()) })
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Exercise errors.As/Is through both wrapping and nested joins.
			err := fmt.Errorf("outer: %w", errors.Join(errors.New("context"), fmt.Errorf("inner: %w", errors.Join(tt.err))))
			got := d.failTopologyErr("request", tt.action, "pane", err)
			if got.OK || got.Phase != tt.phase || got.Error != tt.message || !reflect.DeepEqual(got.Data, tt.data) || got.RequestID != "request" || got.Action != tt.action || got.PaneID != "pane" {
				t.Fatalf("result = %+v; want phase=%s message=%q data=%#v", got, tt.phase, tt.message, tt.data)
			}
		})
	}
}

func TestTopologyMoveBlockRefusal(t *testing.T) {
	d := NewDispatcher(nil, NewState(testLogger()), nil, testLogger())
	t.Cleanup(func() { _ = d.Close(context.Background()) })
	for _, started := range []bool{false, true} {
		for _, partial := range []bool{false, true} {
			t.Run(fmt.Sprintf("started=%t/partial=%t", started, partial), func(t *testing.T) {
				var err error = &herdr.CLIError{Code: "workspace_move_block_failed", Message: " selection changed "}
				if started {
					err = &herdr.OutcomeError{Started: true, Err: err}
				}
				if partial {
					err = errors.Join(herdr.ErrPartiallyApplied, err)
				}
				got := d.failTopologyErr("r", "workspace_reorder", "", fmt.Errorf("wrapped: %w", errors.Join(errors.New("context"), err)))
				if partial {
					if got.Phase != "dispatched_unknown" || !reflect.DeepEqual(got.Data, map[string]any{"dispatched_unknown": true}) {
						t.Fatalf("partial result = %+v", got)
					}
				} else if got.Phase != "not_started" || got.Error != "selection changed" || !reflect.DeepEqual(got.Data, map[string]any{"code": "workspace_move_block_failed"}) {
					t.Fatalf("refusal result = %+v", got)
				}
			})
		}
	}
}

// Partial application is an explicit safety override, including read/list errors.
func TestTopologyPartialOutcomePrecedence(t *testing.T) {
	d := NewDispatcher(nil, NewState(testLogger()), nil, testLogger())
	t.Cleanup(func() { _ = d.Close(context.Background()) })
	for _, action := range []string{"workspace_reorder", "worktree_list"} {
		for _, cause := range []error{
			&herdr.CLIError{Code: "dirty_worktree_requires_force", Message: "dirty"},
			&herdr.CLIError{Code: "invalid_request", Message: "refused"},
			&herdr.CLIError{Code: "workspace_move_block_failed", Message: "refused"},
			herdr.ErrNotStarted, context.DeadlineExceeded, herdr.ErrCreatedTargetUnknown,
		} {
			err := fmt.Errorf("outer: %w", errors.Join(herdr.ErrPartiallyApplied, fmt.Errorf("inner: %w", cause)))
			got := d.failTopologyErr("r", action, "", err)
			if got.OK || got.Phase != "dispatched_unknown" || got.Error != "Part of the command already reached the agent; review it before retrying" || !reflect.DeepEqual(got.Data, map[string]any{"dispatched_unknown": true}) {
				t.Fatalf("%s / %v: %+v", action, cause, got)
			}
		}
	}
}

func TestTopologyMoveBlockSocketRefusal(t *testing.T) {
	socket := filepath.Join(testSocketDir(t), "sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		var request struct {
			ID     string `json:"id"`
			Method string `json:"method"`
			Params struct {
				WorkspaceIDs []string `json:"workspace_ids"`
				Before       string   `json:"before_workspace_id"`
			} `json:"params"`
		}
		if err := json.NewDecoder(conn).Decode(&request); err != nil {
			done <- err
			return
		}
		if request.Method != "workspace.move_block" || !reflect.DeepEqual(request.Params.WorkspaceIDs, []string{"w1"}) || request.Params.Before != "w2" {
			done <- fmt.Errorf("unexpected request: %+v", request)
			return
		}
		done <- json.NewEncoder(conn).Encode(map[string]any{"id": request.ID, "error": map[string]any{"code": "workspace_move_block_failed", "message": " selection changed "}})
	}()
	client := herdr.NewClient("/binary/must-not-run", socket)
	t.Cleanup(func() { _ = client.Close() })
	d := NewDispatcher(client, NewState(testLogger()), nil, testLogger())
	t.Cleanup(func() { _ = d.Close(context.Background()) })
	d.state.CommitWorkspaces([]herdr.Workspace{{ID: "w1"}, {ID: "w2"}})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	got := d.HandleWorkspaceReorderBlock(ctx, "move-1", []string{"w1"}, "w2")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got.OK || got.Phase != "not_started" || got.Error != "selection changed" || !reflect.DeepEqual(got.Data, map[string]any{"code": "workspace_move_block_failed"}) {
		t.Fatalf("result = %+v", got)
	}
}
