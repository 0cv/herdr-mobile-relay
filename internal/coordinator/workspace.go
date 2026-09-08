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
	commandCtx, cancel := context.WithTimeout(ctx, workspaceCommandDeadline)
	defer cancel()
	d.admitTopology(ctx)
	d.topologyMu.Lock()
	defer d.topologyMu.Unlock()
	created, err := d.herdr.WorkspaceCreate(commandCtx, resolved, label)
	if err != nil {
		return d.failTopologyErr(requestID, action, "", err)
	}
	d.topologyChanged()
	d.recordActivity(action, "created", "Created workspace "+label, "", requestID)
	return completed(requestID, action, "", map[string]any{
		"workspace_id": created.WorkspaceID,
		"pane_id":      created.PaneID,
		"tab_id":       created.TabID,
		"cwd":          resolved,
		"label":        label,
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
	commandCtx, cancel := context.WithTimeout(ctx, workspaceCommandDeadline)
	defer cancel()
	d.admitTopology(ctx)
	d.topologyMu.Lock()
	defer d.topologyMu.Unlock()
	if err := d.herdr.WorkspaceRename(commandCtx, workspace.ID, label); err != nil {
		return d.failTopologyErr(requestID, action, "", err)
	}
	d.topologyChanged()
	d.recordActivity(action, "renamed", "Renamed workspace to "+label, "", requestID)
	return completed(requestID, action, "", map[string]any{"workspace_id": workspace.ID, "label": label})
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
	commandCtx, cancel := context.WithTimeout(ctx, workspaceCommandDeadline)
	defer cancel()
	d.admitTopology(ctx)
	d.topologyMu.Lock()
	defer d.topologyMu.Unlock()
	if err := d.herdr.WorkspaceMove(commandCtx, workspace.ID, *insertIndex); err != nil {
		return d.failTopologyErr(requestID, action, "", err)
	}
	d.topologyChanged()
	d.recordActivity(action, "reordered", "Reordered workspace", "", requestID)
	return completed(requestID, action, "", map[string]any{"workspace_id": workspace.ID, "insert_index": *insertIndex})
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
	commandCtx, cancel := context.WithTimeout(ctx, workspaceCommandDeadline)
	defer cancel()
	d.admitTopology(ctx)
	d.topologyMu.Lock()
	defer d.topologyMu.Unlock()
	if err := d.herdr.WorkspaceMoveBlock(commandCtx, workspaceIDs, beforeWorkspaceID); err != nil {
		return d.failTopologyErr(requestID, action, "", err)
	}
	d.topologyChanged()
	d.recordActivity(action, "reordered", "Reordered workspace group", "", requestID)
	return completed(requestID, action, "", map[string]any{
		"workspace_ids":       workspaceIDs,
		"before_workspace_id": beforeWorkspaceID,
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
	commandCtx, cancel := context.WithTimeout(ctx, workspaceCommandDeadline)
	defer cancel()
	d.admitTopology(ctx)
	d.topologyMu.Lock()
	defer d.topologyMu.Unlock()

	authoritative, err := d.herdr.WorkspaceList(commandCtx)
	if err != nil {
		return d.workspaceCloseRefusal(
			requestID,
			action,
			"workspace_group_validation_unavailable",
			nil,
		)
	}
	current, ok := workspaceByID(authoritative, workspaceID)
	if !ok {
		if closeGroup {
			return d.workspaceCloseRefusal(requestID, action, "workspace_group_changed", nil)
		}
		return d.fail(requestID, action, "", "Workspace is unavailable")
	}
	groupIDs, primaryID := workspaceGroupIDs(authoritative, current)
	if !validWorkspaceGroupIDs(groupIDs, primaryID) {
		return d.workspaceCloseRefusal(
			requestID,
			action,
			"workspace_group_validation_unavailable",
			nil,
		)
	}
	if !closeGroup && current.Worktree != nil && !current.Worktree.IsLinkedWorktree && len(groupIDs) > 1 {
		return d.workspaceCloseRefusal(requestID, action, "workspace_group_close_required", groupIDs)
	}
	if closeGroup {
		if current.Worktree != nil && current.Worktree.IsLinkedWorktree {
			return d.workspaceCloseRefusal(requestID, action, "workspace_group_primary_required", groupIDs)
		}
		if !validExpectedWorkspaceIDs(expectedWorkspaceIDs, primaryID) {
			return d.workspaceCloseRefusal(requestID, action, "workspace_group_consent_invalid", groupIDs)
		}
		if !sameWorkspaceIDSet(expectedWorkspaceIDs, groupIDs) {
			return d.workspaceCloseRefusal(requestID, action, "workspace_group_changed", groupIDs)
		}
	}
	affectedIDs := []string{current.ID}
	if closeGroup {
		affectedIDs = append([]string(nil), groupIDs...)
	}
	if err := d.herdr.WorkspaceClose(commandCtx, current.ID, closeGroup); err != nil {
		return d.failTopologyErr(requestID, action, "", err)
	}
	d.topologyChanged()
	d.recordActivity(action, "closed", "Closed workspace "+current.Label, "", requestID)
	return completed(requestID, action, "", map[string]any{
		"workspace_id":  current.ID,
		"close_group":   closeGroup,
		"workspace_ids": affectedIDs,
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
	if code == "workspace_group_consent_invalid" {
		public = "Workspace group confirmation is invalid; confirm again"
	}
	data := map[string]any{"code": code}
	if len(workspaceIDs) > 0 {
		data["workspace_ids"] = workspaceIDs
	}
	d.recordActivity(action, "failed", strings.ReplaceAll(action, "_", " ")+" failed: "+public, "", requestID)
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

func validExpectedWorkspaceIDs(expected []string, primaryID string) bool {
	return validWorkspaceGroupIDs(expected, primaryID)
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
	commandCtx, cancel := context.WithTimeout(ctx, worktreeCommandDeadline)
	defer cancel()
	d.admitTopology(ctx)
	d.topologyMu.Lock()
	defer d.topologyMu.Unlock()
	created, err := d.herdr.WorktreeCreate(commandCtx, workspace.ID, branch, base, label)
	if err != nil {
		return d.failTopologyErr(requestID, action, "", err)
	}
	d.topologyChanged()
	d.recordActivity(action, "created", "Created worktree "+branch, "", requestID)
	return completed(requestID, action, "", created)
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
	commandCtx, cancel := context.WithTimeout(ctx, worktreeCommandDeadline)
	defer cancel()
	d.admitTopology(ctx)
	d.topologyMu.Lock()
	defer d.topologyMu.Unlock()
	opened, err := d.herdr.WorktreeOpen(commandCtx, workspace.ID, path, branch, label)
	if err != nil {
		return d.failTopologyErr(requestID, action, "", err)
	}
	d.topologyChanged()
	d.recordActivity(action, "opened", "Opened worktree "+opened.Worktree.Label, "", requestID)
	return completed(requestID, action, "", opened)
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
	commandCtx, cancel := context.WithTimeout(ctx, worktreeCommandDeadline)
	defer cancel()
	d.admitTopology(ctx)
	d.topologyMu.Lock()
	defer d.topologyMu.Unlock()
	removed, err := d.herdr.WorktreeRemove(commandCtx, workspace.ID, force)
	if err != nil {
		return d.failTopologyErr(requestID, action, "", err)
	}
	d.topologyChanged()
	d.recordActivity(action, "removed", "Removed worktree "+workspace.Label, "", requestID)
	return completed(requestID, action, "", removed)
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
	phase := "failed"
	public := "Command failed"
	var data any
	var cliErr *herdr.CLIError
	switch {
	case errors.As(err, &cliErr) && cliErr.Code == "dirty_worktree_requires_force":
		phase = "not_started"
		public = "Worktree has uncommitted changes; force removal is required"
		data = map[string]any{"code": cliErr.Code, "force_available": true}
	case errors.As(err, &cliErr) && topologyRefusalCode(cliErr.Code):
		phase = "not_started"
		public = strings.TrimSpace(cliErr.Message)
		switch cliErr.Code {
		case "protocol_mismatch", "workspace_group_close_required", "workspace_group_changed",
			"workspace_group_primary_required", "workspace_group_consent_invalid",
			"workspace_group_validation_unavailable":
			public = herdr.RefusalMessage(cliErr.Code)
			if cliErr.Code == "workspace_group_consent_invalid" {
				public = "Workspace group confirmation is invalid; confirm again"
			}
		}
		if public == "" {
			public = "Herdr refused the command"
		}
		data = map[string]any{"code": cliErr.Code}
	case action == "worktree_list":
		public = "Worktrees could not be listed"
		if errors.As(err, &cliErr) && strings.TrimSpace(cliErr.Message) != "" {
			public = strings.TrimSpace(cliErr.Message)
			data = map[string]any{"code": cliErr.Code}
		}
	case errors.Is(err, herdr.ErrCreatedTargetUnknown), errors.Is(err, herdr.ErrDispatchedUnknown):
		phase = "dispatched_unknown"
		public = "Herdr may have completed this command; refresh before retrying"
		data = map[string]any{"dispatched_unknown": true}
	case errors.Is(err, herdr.ErrNotStarted), errors.Is(err, context.DeadlineExceeded):
		phase = "not_started"
		public = "Command was not sent; retry is safe"
	}
	d.logger.Warn("topology command failed", "action", action, "request_id", requestID, "phase", phase, "error", err)
	if action != "worktree_list" {
		d.recordActivity(action, "failed", strings.ReplaceAll(action, "_", " ")+" failed: "+public, paneID, requestID)
	}
	return &CommandResult{RequestID: requestID, Action: action, OK: false, Phase: phase, Error: public, PaneID: paneID, Data: data}
}

func topologyRefusalCode(code string) bool {
	switch code {
	case "invalid_request", "workspace_not_found", "worktree_not_found", "not_git_worktree",
		"linked_worktree_source", "worktree_operation_in_progress", "protocol_mismatch",
		"workspace_group_close_required", "workspace_group_changed",
		"workspace_group_primary_required", "workspace_group_consent_invalid",
		"workspace_group_validation_unavailable":
		return true
	default:
		return false
	}
}
