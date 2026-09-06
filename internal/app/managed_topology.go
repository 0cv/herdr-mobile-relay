package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/activeruntime"
	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/readiness"
)

func runManagedTopologyCheckpoint(ctx context.Context, cfg *config.Config, acknowledgedEmpty bool) error {
	if err := validateManagedTopologyCheckpointConfig(cfg); err != nil {
		return err
	}
	for {
		if err := runManagedTopologyCheckpointAttempt(ctx, cfg, acknowledgedEmpty); err == nil {
			return nil
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("managed topology recovery helper deadline: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func runManagedTopologyCheckpointAttempt(ctx context.Context, cfg *config.Config, acknowledgedEmpty bool) error {
	root := filepath.Dir(cfg.ActiveRuntimePath)
	command := "snapshot"
	arguments := []string{
		"--config", cfg.TopologyRemoteConfig,
		"--root", root,
		"--ledger", cfg.TopologyLedgerRoot,
		"--session-map", cfg.TopologySessionMap,
		"--shim-directory", cfg.TopologyShimDirectory,
		"--zdotdir", cfg.TopologyZDOTDir,
	}
	if acknowledgedEmpty {
		command = "acknowledge-empty"
		arguments = append(arguments, "--generation", cfg.ActiveGeneration)
	}
	arguments = append([]string{command}, arguments...)
	process := exec.CommandContext(ctx, cfg.TopologyCommitHelper, arguments...)
	process.Dir = root
	process.Env = managedTopologyCheckpointEnvironment(os.Environ())
	process.Stdin = nil
	process.Stdout = io.Discard
	process.Stderr = io.Discard
	process.WaitDelay = 2 * time.Second
	if err := process.Run(); err != nil {
		return errors.New("managed topology recovery helper failed")
	}
	return nil
}

func validateManagedTopologyCheckpointConfig(cfg *config.Config) error {
	return validateManagedTopologyCheckpointConfigWith(os.Lstat, cfg)
}

func validateManagedTopologyCheckpointConfigWith(lstat func(string) (os.FileInfo, error), cfg *config.Config) error {
	if cfg == nil || !normalizedManagedPath(cfg.ActiveRuntimePath) || filepath.Base(cfg.ActiveRuntimePath) != "active-runtime.json" || filepath.Base(filepath.Dir(cfg.ActiveRuntimePath)) != "herdr" {
		return errors.New("managed topology checkpoint requires the exact active runtime root")
	}
	if !normalizedManagedPath(cfg.TopologyCommitHelper) || filepath.Base(cfg.TopologyCommitHelper) != "OuroWorkbenchRemote" {
		return errors.New("managed topology checkpoint helper is invalid")
	}
	if !normalizedManagedPath(cfg.TopologyRemoteConfig) || !normalizedManagedPath(cfg.TopologyLedgerRoot) || !normalizedManagedPath(cfg.TopologySessionMap) || !normalizedManagedPath(cfg.TopologyShimDirectory) || !normalizedManagedPath(cfg.TopologyZDOTDir) {
		return errors.New("managed topology checkpoint context is incomplete")
	}
	if cfg.ActiveGeneration == "" || strings.TrimSpace(cfg.ActiveGeneration) != cfg.ActiveGeneration || strings.ContainsAny(cfg.ActiveGeneration, "/\\") {
		return errors.New("managed topology checkpoint generation is invalid")
	}
	info, err := lstat(cfg.TopologyCommitHelper)
	stat, ok := infoSyscallStat(info)
	if err != nil || !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 || stat.Nlink != 1 || stat.Uid != uint32(os.Getuid()) {
		return errors.New("managed topology checkpoint helper is unavailable or unsafe")
	}
	return nil
}

func normalizedManagedPath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path
}

func infoSyscallStat(info os.FileInfo) (*syscall.Stat_t, bool) {
	if info == nil {
		return nil, false
	}
	value, ok := info.Sys().(*syscall.Stat_t)
	return value, ok
}

func managedTopologyCheckpointEnvironment(environment []string) []string {
	allowed := map[string]bool{"HOME": true, "USER": true, "LOGNAME": true, "SHELL": true, "PATH": true, "TMPDIR": true, "TERM": true, "LANG": true}
	clean := make([]string, 0, len(environment))
	for _, value := range environment {
		key, _, found := strings.Cut(value, "=")
		if found && (allowed[key] || strings.HasPrefix(key, "LC_")) {
			clean = append(clean, value)
		}
	}
	return clean
}

type managedTopologyTarget struct {
	action      string
	paneID      string
	workspaceID string
	profileID   string
}

func managedTopologyTargetName(target managedTopologyTarget) string {
	if target.paneID != "" {
		return target.paneID
	}
	if target.workspaceID != "" {
		return target.workspaceID
	}
	return target.profileID
}

type managedTopologySnapshot struct {
	panes      []activeruntime.TopologyPane
	workspaces []herdr.Workspace
}

type managedTopologyMarker interface {
	Resolve() error
}

type managedTopologyCommit struct {
	target     managedTopologyTarget
	before     managedTopologySnapshot
	marker     managedTopologyMarker
	reconcile  func(context.Context) error
	snapshot   func() (managedTopologySnapshot, bool)
	checkpoint func(context.Context, bool) error
	publish    func([]readiness.Pane, bool) error
}

func (c *managedTopologyCommit) finish(ctx context.Context, result *coordinator.CommandResult) error {
	if c == nil || c.marker == nil || c.reconcile == nil || c.snapshot == nil || c.checkpoint == nil || c.publish == nil {
		return errors.New("managed topology commit is incomplete")
	}
	if err := c.reconcile(ctx); err != nil {
		return fmt.Errorf("reconcile managed topology: %w", err)
	}
	after, ready := c.snapshot()
	if !ready {
		return errors.New("reconciled managed topology is unavailable")
	}
	if err := validateManagedTopologyDelta(c.target, result, c.before.panes, after.panes); err != nil {
		return err
	}
	if result == nil || !result.OK {
		if result != nil && result.Phase != "dispatched_unknown" && reflect.DeepEqual(c.before, after) {
			return c.marker.Resolve()
		}
		return errors.New("managed topology command outcome requires explicit recovery")
	}
	acknowledgedEmpty := len(after.panes) == 0
	if err := c.checkpoint(ctx, acknowledgedEmpty); err != nil {
		return fmt.Errorf("capture managed topology recovery checkpoint: %w", err)
	}
	expected := make([]readiness.Pane, len(after.panes))
	for index, pane := range after.panes {
		expected[index] = readiness.Pane{PaneID: pane.PaneID, NativeSessionID: pane.NativeSessionID, ProfileID: pane.ProfileID}
	}
	if err := c.publish(expected, acknowledgedEmpty); err != nil {
		return fmt.Errorf("publish managed expected inventory: %w", err)
	}
	return c.marker.Resolve()
}

func managedTopologyAction(action string) bool {
	switch action {
	case "agent_start", "agent_stop", "agent_clear", "agent_restart", "workspace_create", "workspace_rename", "workspace_reorder", "workspace_close", "worktree_create", "worktree_open", "worktree_remove":
		return true
	default:
		return false
	}
}

func managedTopologySnapshotFromState(state *coordinator.State) (managedTopologySnapshot, bool) {
	agents, ready := state.SnapshotAtReadyInventory()
	if !ready {
		return managedTopologySnapshot{}, false
	}
	panes := make([]activeruntime.TopologyPane, len(agents))
	for index, agent := range agents {
		panes[index] = activeruntime.TopologyPane{PaneID: agent.PaneID, NativeSessionID: agent.SessionID, ProfileID: agent.ProfileID, WorkspaceID: agent.WorkspaceID}
	}
	if _, err := managedTopologyPaneIndex(panes); err != nil {
		return managedTopologySnapshot{}, false
	}
	sort.Slice(panes, func(left, right int) bool { return panes[left].PaneID < panes[right].PaneID })
	return managedTopologySnapshot{panes: panes, workspaces: state.Workspaces()}, true
}

func validateManagedTopologyDelta(target managedTopologyTarget, result *coordinator.CommandResult, before, after []activeruntime.TopologyPane) error {
	resultActionMatches := result != nil && (result.Action == "" || result.Action == target.action || target.action == "agent_restart" && result.Action == "agent_clear")
	if !resultActionMatches {
		return errors.New("managed topology result identity is invalid")
	}
	beforeByPane, err := managedTopologyPaneIndex(before)
	if err != nil {
		return fmt.Errorf("invalid prior managed topology: %w", err)
	}
	afterByPane, err := managedTopologyPaneIndex(after)
	if err != nil {
		return fmt.Errorf("invalid reconciled managed topology: %w", err)
	}
	removed := make(map[string]activeruntime.TopologyPane)
	added := make(map[string]activeruntime.TopologyPane)
	for paneID, pane := range beforeByPane {
		current, exists := afterByPane[paneID]
		if !exists || current != pane {
			removed[paneID] = pane
		}
	}
	for paneID, pane := range afterByPane {
		prior, exists := beforeByPane[paneID]
		if !exists || prior != pane {
			added[paneID] = pane
		}
	}
	possibleEffect := result.OK || result.Phase == "dispatched_unknown" || target.action == "agent_start" && result.PaneID != ""
	if !possibleEffect {
		if len(removed) == 0 && len(added) == 0 {
			return nil
		}
		return errors.New("managed topology changed after a command proven not to have applied")
	}

	switch target.action {
	case "agent_start":
		if len(removed) != 0 || len(added) > 1 || result.OK && len(added) != 1 {
			return errors.New("agent start produced an unexpected fleet delta")
		}
		for paneID, pane := range added {
			if target.profileID == "" || pane.ProfileID != target.profileID || result.PaneID != "" && paneID != result.PaneID {
				return errors.New("agent start produced the wrong pane or profile")
			}
		}
	case "agent_stop":
		if len(added) != 0 || len(removed) > 1 || result.OK && len(removed) != 1 {
			return errors.New("agent stop produced an unexpected fleet delta")
		}
		for paneID := range removed {
			if paneID != target.paneID {
				return errors.New("agent stop removed an unrelated pane")
			}
		}
	case "agent_clear", "agent_restart":
		prior, exists := beforeByPane[target.paneID]
		if !exists || len(removed) > 1 || len(added) > 1 || result.OK && len(added) != 1 {
			return errors.New("agent replacement produced an unexpected fleet delta")
		}
		for paneID := range removed {
			if paneID != target.paneID {
				return errors.New("agent replacement removed an unrelated pane")
			}
		}
		for _, pane := range added {
			if pane.ProfileID != prior.ProfileID {
				return errors.New("agent replacement crossed profile ownership")
			}
		}
		if result.OK && result.Phase != "completed_with_warning" {
			if _, removedTarget := removed[target.paneID]; !removedTarget {
				return errors.New("agent replacement retained the old target without a warning")
			}
		}
	case "workspace_close", "worktree_remove":
		if len(added) != 0 || target.workspaceID == "" {
			return errors.New("workspace removal produced an unexpected fleet delta")
		}
		for _, pane := range removed {
			if pane.WorkspaceID != target.workspaceID {
				return errors.New("workspace removal affected another workspace")
			}
		}
	case "workspace_create", "workspace_rename", "workspace_reorder", "worktree_list", "worktree_create", "worktree_open":
		if len(removed) != 0 || len(added) != 0 {
			return errors.New("metadata topology command changed the agent fleet")
		}
	default:
		return errors.New("unsupported managed topology action")
	}
	return nil
}

func managedTopologyPaneIndex(panes []activeruntime.TopologyPane) (map[string]activeruntime.TopologyPane, error) {
	result := make(map[string]activeruntime.TopologyPane, len(panes))
	nativeSessions := make(map[string]bool, len(panes))
	for _, pane := range panes {
		if pane.PaneID == "" || pane.NativeSessionID == "" || pane.ProfileID == "" || result[pane.PaneID].PaneID != "" || nativeSessions[pane.NativeSessionID] {
			return nil, errors.New("duplicate or incomplete pane identity")
		}
		result[pane.PaneID] = pane
		nativeSessions[pane.NativeSessionID] = true
	}
	return result, nil
}
