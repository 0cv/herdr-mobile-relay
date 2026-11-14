package coordinator

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

const (
	workspaceCommandDeadline = 20 * time.Second
	worktreeCommandDeadline  = 60 * time.Second
	workspaceLabelMaxRunes   = 128
	worktreeValueMaxRunes    = 512
	maxWorkspaceGroupIDs     = 256
	maxWorkspaceIDRunes      = 256
)

func (d *Dispatcher) HandleWorkspaceCreate(
	ctx context.Context,
	requestID, cwd, label string,
) *CommandResult {
	const action = "workspace_create"
	label = strings.TrimSpace(label)
	if err := validWorkspaceLabel(label); err != nil {
		return d.fail(requestID, action, "", err.Error())
	}
	if d.lifecycle == nil {
		return d.fail(requestID, action, "", "Agent profiles are unavailable")
	}
	resolved, err := d.lifecycle.ResolveCwd(cwd)
	if err != nil {
		return d.fail(requestID, action, "", err.Error())
	}
	return d.mutateTopology(ctx, requestID, topologyMutation{
		action:  action,
		timeout: workspaceCommandDeadline,
		status:  "created",
		run: func(commandCtx context.Context) (topologyDone, error) {
			created, err := d.herdr.WorkspaceCreate(commandCtx, resolved, label)
			if err != nil {
				return topologyDone{}, err
			}
			return topologyDone{
				summary: "Created workspace " + label,
				data: map[string]any{
					"workspace_id": created.WorkspaceID,
					"pane_id":      created.PaneID,
					"tab_id":       created.TabID,
					"cwd":          resolved,
					"label":        label,
				},
			}, nil
		},
	})
}

func (d *Dispatcher) HandleWorkspaceRename(
	ctx context.Context,
	requestID, workspaceID, label string,
) *CommandResult {
	const action = "workspace_rename"
	workspace, result := d.workspaceTarget(requestID, action, workspaceID)
	if result != nil {
		return result
	}
	label = strings.TrimSpace(label)
	if err := validWorkspaceLabel(label); err != nil {
		return d.fail(requestID, action, "", err.Error())
	}
	return d.mutateTopology(ctx, requestID, topologyMutation{
		action:  action,
		timeout: workspaceCommandDeadline,
		status:  "renamed",
		run: func(commandCtx context.Context) (topologyDone, error) {
			if err := d.herdr.WorkspaceRename(commandCtx, workspace.ID, label); err != nil {
				return topologyDone{}, err
			}
			return topologyDone{
				summary: "Renamed workspace to " + label,
				data:    map[string]any{"workspace_id": workspace.ID, "label": label},
			}, nil
		},
	})
}

func (d *Dispatcher) HandleWorkspaceReorder(
	ctx context.Context,
	requestID, workspaceID string,
	insertIndex *int,
) *CommandResult {
	const action = "workspace_reorder"
	workspace, result := d.workspaceTarget(requestID, action, workspaceID)
	if result != nil {
		return result
	}
	if insertIndex == nil || *insertIndex < 0 || *insertIndex > maxTabInsertIndex {
		return d.fail(requestID, action, "", "Workspace position is invalid")
	}
	return d.mutateTopology(ctx, requestID, topologyMutation{
		action:  action,
		timeout: workspaceCommandDeadline,
		status:  "reordered",
		run: func(commandCtx context.Context) (topologyDone, error) {
			if err := d.herdr.WorkspaceMove(commandCtx, workspace.ID, *insertIndex); err != nil {
				return topologyDone{}, err
			}
			return topologyDone{
				summary: "Reordered workspace",
				data:    map[string]any{"workspace_id": workspace.ID, "insert_index": *insertIndex},
			}, nil
		},
	})
}

func (d *Dispatcher) HandleWorkspaceReorderBlock(
	ctx context.Context,
	requestID string,
	workspaceIDs []string,
	beforeWorkspaceID string,
) *CommandResult {
	const action = "workspace_reorder"
	if len(workspaceIDs) == 0 || len(workspaceIDs) > maxTabInsertIndex {
		return d.fail(requestID, action, "", "Workspace selection is invalid")
	}
	seen := make(map[string]bool, len(workspaceIDs))
	for _, workspaceID := range workspaceIDs {
		if workspaceID == "" || seen[workspaceID] {
			return d.fail(requestID, action, "", "Workspace selection is invalid")
		}
		if _, ok := d.state.Workspace(workspaceID); !ok {
			return d.fail(requestID, action, "", "Workspace is unavailable")
		}
		seen[workspaceID] = true
	}
	if beforeWorkspaceID != "" {
		if seen[beforeWorkspaceID] {
			return d.fail(requestID, action, "", "Workspace destination is invalid")
		}
		if _, ok := d.state.Workspace(beforeWorkspaceID); !ok {
			return d.fail(requestID, action, "", "Workspace destination is unavailable")
		}
	}
	return d.mutateTopology(ctx, requestID, topologyMutation{
		action:  action,
		timeout: workspaceCommandDeadline,
		status:  "reordered",
		run: func(commandCtx context.Context) (topologyDone, error) {
			if err := d.herdr.WorkspaceMoveBlock(commandCtx, workspaceIDs, beforeWorkspaceID); err != nil {
				return topologyDone{}, err
			}
			return topologyDone{
				summary: "Reordered workspace group",
				data: map[string]any{
					"workspace_ids":       workspaceIDs,
					"before_workspace_id": beforeWorkspaceID,
				},
			}, nil
		},
	})
}

func (d *Dispatcher) HandleWorkspaceClose(
	ctx context.Context,
	requestID, workspaceID string,
	closeGroup bool,
	expectedWorkspaceIDs []string,
) *CommandResult {
	const action = "workspace_close"
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return d.fail(requestID, action, "", "Workspace is required")
	}
	return d.mutateTopology(ctx, requestID, topologyMutation{
		action:  action,
		timeout: workspaceCommandDeadline,
		status:  "closed",
		run: func(commandCtx context.Context) (topologyDone, error) {
			authoritative, err := d.herdr.WorkspaceList(commandCtx)
			if err != nil {
				return topologyDone{result: d.workspaceCloseRefusal(
					requestID,
					action,
					"workspace_group_validation_unavailable",
					nil,
				)}, nil
			}
			current, ok := workspaceByID(authoritative, workspaceID)
			if !ok {
				if closeGroup {
					return topologyDone{result: d.workspaceCloseRefusal(requestID, action, "workspace_group_changed", nil)}, nil
				}
				return topologyDone{result: d.fail(requestID, action, "", "Workspace is unavailable")}, nil
			}
			groupIDs, primaryID := workspaceGroupIDs(authoritative, current)
			if !validWorkspaceGroupIDs(groupIDs, primaryID) {
				return topologyDone{result: d.workspaceCloseRefusal(
					requestID,
					action,
					"workspace_group_validation_unavailable",
					nil,
				)}, nil
			}
			if !closeGroup && current.Worktree != nil && !current.Worktree.IsLinkedWorktree && len(groupIDs) > 1 {
				return topologyDone{result: d.workspaceCloseRefusal(requestID, action, "workspace_group_close_required", groupIDs)}, nil
			}
			if closeGroup {
				// These IDs are a relay-level confirmation check only. Herdr 0.9.0
				// closes the group that exists when its close_group mutation executes;
				// membership added after this list cannot be excluded atomically.
				if current.Worktree != nil && current.Worktree.IsLinkedWorktree {
					return topologyDone{result: d.workspaceCloseRefusal(requestID, action, "workspace_group_primary_required", groupIDs)}, nil
				}
				if !validWorkspaceGroupIDs(expectedWorkspaceIDs, primaryID) {
					return topologyDone{result: d.workspaceCloseRefusal(requestID, action, "workspace_group_consent_invalid", groupIDs)}, nil
				}
				if !sameWorkspaceIDSet(expectedWorkspaceIDs, groupIDs) {
					return topologyDone{result: d.workspaceCloseRefusal(requestID, action, "workspace_group_changed", groupIDs)}, nil
				}
			}
			affectedIDs := []string{current.ID}
			if closeGroup {
				affectedIDs = append([]string(nil), groupIDs...)
			}
			if err := d.herdr.WorkspaceClose(commandCtx, current.ID, closeGroup); err != nil {
				return topologyDone{}, err
			}
			return topologyDone{
				summary: "Closed workspace " + current.Label,
				data: map[string]any{
					"workspace_id":  current.ID,
					"close_group":   closeGroup,
					"workspace_ids": affectedIDs,
				},
			}, nil
		},
	})
}

func workspaceByID(workspaces []herdr.Workspace, workspaceID string) (herdr.Workspace, bool) {
	for _, workspace := range workspaces {
		if workspace.ID == workspaceID {
			return workspace, true
		}
	}
	return herdr.Workspace{}, false
}

func (d *Dispatcher) workspaceCloseRefusal(
	requestID, action, code string, workspaceIDs []string,
) *CommandResult {
	public := herdr.RefusalMessage(code)
	data := map[string]any{"code": code}
	if len(workspaceIDs) > 0 {
		data["workspace_ids"] = workspaceIDs
	}
	d.recordActivity(action, "failed", failureSummary(action, public), "", requestID)
	return &CommandResult{
		RequestID: requestID,
		Action:    action,
		OK:        false,
		Phase:     "not_started",
		Error:     public,
		Data:      data,
	}
}

func workspaceGroupIDs(workspaces []herdr.Workspace, selected herdr.Workspace) ([]string, string) {
	if selected.Worktree == nil || selected.Worktree.RepoKey == "" {
		return []string{selected.ID}, selected.ID
	}
	repoKey := selected.Worktree.RepoKey
	primaryID := selected.ID
	for _, workspace := range workspaces {
		if workspace.Worktree == nil || workspace.Worktree.RepoKey != repoKey {
			continue
		}
		if !workspace.Worktree.IsLinkedWorktree {
			primaryID = workspace.ID
			break
		}
	}
	group := make([]string, 0, len(workspaces))
	for _, workspace := range workspaces {
		if workspace.Worktree != nil && workspace.Worktree.RepoKey == repoKey {
			group = append(group, workspace.ID)
		}
	}
	if len(group) == 0 {
		return []string{selected.ID}, primaryID
	}
	return group, primaryID
}

func validWorkspaceGroupIDs(ids []string, primaryID string) bool {
	if len(ids) == 0 || len(ids) > maxWorkspaceGroupIDs || primaryID == "" {
		return false
	}
	seen := make(map[string]struct{}, len(ids))
	for _, workspaceID := range ids {
		if utf8.RuneCountInString(workspaceID) > maxWorkspaceIDRunes ||
			workspaceID == "" || workspaceID != strings.TrimSpace(workspaceID) {
			return false
		}
		if _, ok := seen[workspaceID]; ok {
			return false
		}
		seen[workspaceID] = struct{}{}
	}
	_, ok := seen[primaryID]
	return ok
}

func sameWorkspaceIDSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	seen := make(map[string]struct{}, len(left))
	for _, workspaceID := range left {
		seen[workspaceID] = struct{}{}
	}
	for _, workspaceID := range right {
		if _, ok := seen[workspaceID]; !ok {
			return false
		}
	}
	return true
}

func (d *Dispatcher) HandleWorktreeList(
	ctx context.Context,
	requestID, workspaceID string,
) *CommandResult {
	const action = "worktree_list"
	workspace, result := d.workspaceTarget(requestID, action, workspaceID)
	if result != nil {
		return result
	}
	commandCtx, cancel := context.WithTimeout(ctx, workspaceCommandDeadline)
	defer cancel()
	listing, err := d.herdr.WorktreeList(commandCtx, workspace.ID)
	if err != nil {
		return d.failTopologyErr(requestID, action, "", err)
	}
	return completed(requestID, action, "", listing)
}

func (d *Dispatcher) HandleWorktreeCreate(
	ctx context.Context,
	requestID, workspaceID, branch, base, path, label string,
) *CommandResult {
	const action = "worktree_create"
	workspace, result := d.workspaceTarget(requestID, action, workspaceID)
	if result != nil {
		return result
	}
	branch = strings.TrimSpace(branch)
	base = strings.TrimSpace(base)
	path = strings.TrimSpace(path)
	label = strings.TrimSpace(label)
	if branch == "" {
		return d.fail(requestID, action, "", "Branch is required")
	}
	if path != "" {
		return d.fail(requestID, action, "", "Custom worktree paths are not available from the phone")
	}
	if err := validWorktreeValues(branch, base, path, label); err != nil {
		return d.fail(requestID, action, "", err.Error())
	}
	return d.mutateTopology(ctx, requestID, topologyMutation{
		action:  action,
		timeout: worktreeCommandDeadline,
		status:  "created",
		run: func(commandCtx context.Context) (topologyDone, error) {
			created, err := d.herdr.WorktreeCreate(commandCtx, workspace.ID, branch, base, label)
			if err != nil {
				return topologyDone{}, err
			}
			return topologyDone{summary: "Created worktree " + branch, data: created}, nil
		},
	})
}

func (d *Dispatcher) HandleWorktreeOpen(
	ctx context.Context,
	requestID, workspaceID, path, branch, label string,
) *CommandResult {
	const action = "worktree_open"
	workspace, result := d.workspaceTarget(requestID, action, workspaceID)
	if result != nil {
		return result
	}
	path = strings.TrimSpace(path)
	branch = strings.TrimSpace(branch)
	label = strings.TrimSpace(label)
	if (path == "") == (branch == "") {
		return d.fail(requestID, action, "", "Choose exactly one worktree path or branch")
	}
	if err := validWorktreeValues(branch, "", path, label); err != nil {
		return d.fail(requestID, action, "", err.Error())
	}
	return d.mutateTopology(ctx, requestID, topologyMutation{
		action:  action,
		timeout: worktreeCommandDeadline,
		status:  "opened",
		run: func(commandCtx context.Context) (topologyDone, error) {
			opened, err := d.herdr.WorktreeOpen(commandCtx, workspace.ID, path, branch, label)
			if err != nil {
				return topologyDone{}, err
			}
			return topologyDone{summary: "Opened worktree " + opened.Worktree.Label, data: opened}, nil
		},
	})
}

func (d *Dispatcher) HandleWorktreeRemove(
	ctx context.Context,
	requestID, workspaceID string,
	force bool,
) *CommandResult {
	const action = "worktree_remove"
	workspace, result := d.workspaceTarget(requestID, action, workspaceID)
	if result != nil {
		return result
	}
	if workspace.Worktree == nil || !workspace.Worktree.IsLinkedWorktree {
		return d.fail(requestID, action, "", "Workspace is not a removable linked worktree")
	}
	return d.mutateTopology(ctx, requestID, topologyMutation{
		action:  action,
		timeout: worktreeCommandDeadline,
		status:  "removed",
		run: func(commandCtx context.Context) (topologyDone, error) {
			removed, err := d.herdr.WorktreeRemove(commandCtx, workspace.ID, force)
			if err != nil {
				return topologyDone{}, err
			}
			return topologyDone{summary: "Removed worktree " + workspace.Label, data: removed}, nil
		},
	})
}

func (d *Dispatcher) workspaceTarget(
	requestID, action, workspaceID string,
) (herdr.Workspace, *CommandResult) {
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return herdr.Workspace{}, d.fail(requestID, action, "", "Workspace is required")
	}
	workspace, ok := d.state.Workspace(workspaceID)
	if !ok {
		return herdr.Workspace{}, d.fail(requestID, action, "", "Workspace is unavailable")
	}
	return workspace, nil
}

type topologyMutation struct {
	action  string
	timeout time.Duration
	status  string
	run     func(context.Context) (topologyDone, error)
}

// topologyDone is what a successful mutation reports. A non-nil result is
// returned as is, for refusals decided under the topology lock.
type topologyDone struct {
	summary string
	data    any
	result  *CommandResult
}

func (d *Dispatcher) mutateTopology(ctx context.Context, requestID string, mutation topologyMutation) *CommandResult {
	commandCtx, cancel := context.WithTimeout(ctx, mutation.timeout)
	defer cancel()
	d.admitTopology(ctx)
	d.topologyMu.Lock()
	defer d.topologyMu.Unlock()
	done, err := mutation.run(commandCtx)
	if err != nil {
		return d.failTopologyErr(requestID, mutation.action, "", err)
	}
	if done.result != nil {
		return done.result
	}
	d.topologyChanged()
	d.recordActivity(mutation.action, mutation.status, done.summary, "", requestID)
	return completed(requestID, mutation.action, "", done.data)
}

func (d *Dispatcher) topologyChanged() {
	d.state.MarkTopologyChanged()
	if d.wakePoll != nil {
		d.wakePoll()
	}
}

// admitTopology unblocks the hub's global ordered ingress before the caller
// waits on topologyMu: the mutex serializes the Herdr topology commands
// themselves, but neither the wait for it nor the command may keep later
// unrelated messages from being admitted — a queued second mutation used to
// stall every client's ingress for the running command's full deadline.
// Arrival order between concurrently queued topology mutations is not
// preserved; each handler validates against the current topology, and a
// single phone serializes its own mutations anyway. No-op outside
// HandleTopologyAdmitted.
func (d *Dispatcher) admitTopology(ctx context.Context) {
	if admitted, ok := ctx.Value(admissionContextKey{}).(func()); ok && admitted != nil {
		admitted()
	}
}

func validWorkspaceLabel(label string) error {
	if label == "" {
		return errors.New("Workspace label is required")
	}
	if utf8.RuneCountInString(label) > workspaceLabelMaxRunes {
		return errors.New("Workspace label is too long")
	}
	return nil
}

func validWorktreeValues(values ...string) error {
	for _, value := range values {
		if utf8.RuneCountInString(value) > worktreeValueMaxRunes {
			return errors.New("Worktree value is too long")
		}
	}
	return nil
}

func (d *Dispatcher) failTopologyErr(requestID, action, paneID string, err error) *CommandResult {
	class := classifyFailure(err)
	phase := class.phase()
	public := "Command failed"
	var data any
	var cliErr *herdr.CLIError
	isCLI := errors.As(err, &cliErr)
	switch {
	case class.kind == failurePartiallyApplied:
		public = partiallyAppliedMessage
	case class.kind == failureRefused && class.code == "dirty_worktree_requires_force":
		public = "Worktree has uncommitted changes; force removal is required"
		data = map[string]any{"code": class.code, "force_available": true}
	case class.kind == failureRefused:
		public = topologyRefusalMessage(class.code, cliErr.Message)
		data = map[string]any{"code": class.code}
	case action == "worktree_list":
		phase = "failed"
		public = "Worktrees could not be listed"
		if isCLI && strings.TrimSpace(cliErr.Message) != "" {
			public = strings.TrimSpace(cliErr.Message)
			data = map[string]any{"code": cliErr.Code}
		}
	case class.kind == failureCreatedTargetUnknown, class.kind == failureDispatchedUnknown:
		public = "Herdr may have completed this command; refresh before retrying"
	case class.kind == failureNotStarted:
		public = notSentRetrySafe
	}
	if phase == "dispatched_unknown" {
		data = uncertainOutcomeData()
	}
	d.logger.Warn("topology command failed", "action", action, "request_id", requestID, "phase", phase, "error", err)
	if action != "worktree_list" {
		d.recordActivity(action, "failed", failureSummary(action, public), paneID, requestID)
	}
	return &CommandResult{RequestID: requestID, Action: action, OK: false, Phase: phase, Error: public, PaneID: paneID, Data: data}
}

func topologyRefusalMessage(code, message string) string {
	switch code {
	case "protocol_mismatch", "workspace_group_close_required", "workspace_group_changed",
		"workspace_group_primary_required", "workspace_group_consent_invalid",
		"workspace_group_validation_unavailable":
		return herdr.RefusalMessage(code)
	}
	if message = strings.TrimSpace(message); message != "" {
		return message
	}
	return "Herdr refused the command"
}
