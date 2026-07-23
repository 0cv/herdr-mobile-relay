package coordinator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/profiles"
)

const (
	agentStartProcessTimeoutMS = 30000
	agentStartCleanupReserve   = 5 * time.Second
	customAgentPollInterval    = 250 * time.Millisecond
)

var agentNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

type StartRequest struct {
	ProfileID string
	Name      string
	Cwd       string
	Prompt    string
}

type StartResult struct {
	PaneID string `json:"pane_id"`
	Name   string `json:"name"`
	Cwd    string `json:"cwd"`
}

type Lifecycle struct {
	herdr    *herdr.Client
	profiles *profiles.Resolver
	home     string
}

func NewLifecycle(client *herdr.Client, resolver *profiles.Resolver) *Lifecycle {
	home, _ := os.UserHomeDir()
	return &Lifecycle{herdr: client, profiles: resolver, home: home}
}

func (l *Lifecycle) ValidateStart(request StartRequest) (profiles.Profile, StartRequest, error) {
	profile, ok := l.profiles.Profile(request.ProfileID)
	if !ok {
		return profiles.Profile{}, request, errors.New("profile_id is not available")
	}
	if !agentNamePattern.MatchString(request.Name) {
		return profiles.Profile{}, request, errors.New("name must match [a-z][a-z0-9_-]{0,31}")
	}
	if len([]rune(request.Prompt)) > promptMaxChars {
		return profiles.Profile{}, request, errors.New("prompt exceeds maximum length")
	}
	cwd, err := l.resolveCwd(request.Cwd)
	if err != nil {
		return profiles.Profile{}, request, err
	}
	request.Cwd = cwd
	return profile, request, nil
}

func (l *Lifecycle) Start(ctx context.Context, profile profiles.Profile, request StartRequest) (StartResult, error) {
	if existing := l.reconcileExisting(ctx, profile.ID, request); existing != "" {
		l.profiles.Remember(existing, profile.ID)
		return StartResult{PaneID: existing, Name: request.Name, Cwd: request.Cwd}, nil
	}

	deadline, ok := ctx.Deadline()
	if !ok {
		return StartResult{}, errors.New("agent start requires an absolute deadline")
	}
	startupDeadline := deadline.Add(-agentStartCleanupReserve)
	if !time.Now().Before(startupDeadline) {
		return StartResult{}, herdr.ErrNotStarted
	}
	startupCtx, cancel := context.WithDeadline(ctx, startupDeadline)
	defer cancel()

	inventory, err := l.herdr.GetInventory(startupCtx)
	if err != nil {
		return StartResult{}, err
	}
	workspaces, err := l.herdr.WorkspaceList(startupCtx)
	if err != nil {
		return StartResult{}, err
	}
	workspaceID := SelectWorkspaceForCwd(request.Cwd, inventory.Panes, workspaces, l.home)

	target, err := l.createTarget(startupCtx, workspaceID, request.Name, request.Cwd)
	if err != nil {
		return StartResult{}, err
	}

	startErr := l.startInTarget(startupCtx, profile, request.Name, target.PaneID)
	if startErr != nil {
		return StartResult{}, l.cleanupFailedTarget(ctx, target.PaneID, startErr)
	}

	l.profiles.Remember(target.PaneID, profile.ID)
	return StartResult{PaneID: target.PaneID, Name: request.Name, Cwd: request.Cwd}, nil
}

func (l *Lifecycle) createTarget(ctx context.Context, workspaceID, label, cwd string) (*herdr.CreateResult, error) {
	if workspaceID != "" {
		return l.herdr.TabCreate(ctx, workspaceID, cwd, label)
	}
	workspaceLabel := filepath.Base(cwd)
	if workspaceLabel == "." || workspaceLabel == string(filepath.Separator) || workspaceLabel == "" {
		workspaceLabel = "workspace"
	}
	result, err := l.herdr.WorkspaceCreate(ctx, cwd, workspaceLabel)
	if err != nil {
		return nil, err
	}
	if result.TabID == "" {
		return result, nil
	}
	if err := l.herdr.TabRename(ctx, result.TabID, label); err != nil {
		_ = l.herdr.StopPane(ctx, result.PaneID)
		return nil, fmt.Errorf("label new tab: %w", err)
	}
	return result, nil
}

func (l *Lifecycle) startInTarget(ctx context.Context, profile profiles.Profile, name, paneID string) error {
	if profile.Kind != "" {
		_, err := l.herdr.StartAgent(ctx, name, profile.Kind, paneID, agentStartProcessTimeoutMS)
		return err
	}
	if len(profile.Argv) == 0 {
		return errors.New("profile has no executable argv")
	}
	if err := l.herdr.PaneRun(ctx, paneID, profile.Argv); err != nil {
		return err
	}
	ticker := time.NewTicker(customAgentPollInterval)
	defer ticker.Stop()
	for {
		info, err := l.herdr.AgentGet(ctx, paneID)
		if err == nil && (info.Running || info.Status != "") {
			return l.herdr.RenameAgent(ctx, paneID, name)
		}
		select {
		case <-ctx.Done():
			// PaneRun completed successfully, so the profile command was
			// dispatched even though its eventual agent state is unknown.
			return fmt.Errorf("%w: wait for custom agent: %v", herdr.ErrDispatchedUnknown, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (l *Lifecycle) cleanupFailedTarget(ctx context.Context, paneID string, startErr error) error {
	if paneID == "" {
		return startErr
	}
	if err := l.herdr.StopPane(ctx, paneID); err != nil {
		return fmt.Errorf("%w; unused target cleanup failed: %v", startErr, err)
	}
	l.profiles.Forget(paneID)
	return startErr
}

func (l *Lifecycle) reconcileExisting(ctx context.Context, profileID string, request StartRequest) string {
	inventory, err := l.herdr.GetInventory(ctx)
	if err != nil {
		return ""
	}
	for _, pane := range inventory.Panes {
		if pane.Name != request.Name {
			continue
		}
		cwd, err := filepath.EvalSymlinks(pane.Cwd)
		if err != nil || cwd != request.Cwd {
			continue
		}
		if l.profiles.ResolvePane(pane.ID, pane.Agent) == profileID {
			return pane.ID
		}
	}
	return ""
}

func (l *Lifecycle) resolveCwd(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("cwd is required")
	}
	home, err := filepath.Abs(l.home)
	if err != nil {
		return "", errors.New("home directory is unavailable")
	}
	absolute, err := filepath.Abs(raw)
	if err != nil {
		return "", errors.New("cwd is invalid")
	}
	relative, err := filepath.Rel(home, absolute)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("cwd must be inside the home directory")
	}
	if relative == "." {
		return "", errors.New("cwd must be a project directory below the home directory")
	}
	root, err := os.OpenRoot(home)
	if err != nil {
		return "", errors.New("home directory is unavailable")
	}
	defer root.Close()
	directory, err := root.Open(relative)
	if err != nil {
		return "", errors.New("cwd is not an accessible directory inside the home directory")
	}
	defer directory.Close()
	info, err := directory.Stat()
	if err != nil || !info.IsDir() {
		return "", errors.New("cwd is not an accessible directory")
	}
	return absolute, nil
}

// SelectWorkspaceForCwd freezes the label/exclusive/majority heuristic used by
// the Python reference. Ambiguous candidates deliberately return no match.
func SelectWorkspaceForCwd(cwd string, panes []herdr.Pane, workspaces []herdr.Workspace, home string) string {
	target, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return ""
	}
	type counts struct{ matching, total int }
	byWorkspace := make(map[string]counts)
	for _, pane := range panes {
		if pane.WorkspaceID == "" {
			continue
		}
		count := byWorkspace[pane.WorkspaceID]
		count.total++
		paneCwd, err := filepath.EvalSymlinks(pane.Cwd)
		if err == nil && paneCwd == target {
			count.matching++
		}
		byWorkspace[pane.WorkspaceID] = count
	}
	candidates := make(map[string]bool)
	for workspaceID, count := range byWorkspace {
		if count.matching > 0 {
			candidates[workspaceID] = true
		}
	}
	if len(candidates) == 0 {
		return ""
	}

	labels := map[string]bool{filepath.Base(target): true}
	if resolvedHome, err := filepath.EvalSymlinks(home); err == nil && target == resolvedHome {
		labels["~"] = true
	}
	var labelled []string
	for _, workspace := range workspaces {
		if candidates[workspace.ID] && labels[workspace.Label] {
			labelled = append(labelled, workspace.ID)
		}
	}
	if len(labelled) == 1 {
		return labelled[0]
	}

	var exclusive []string
	for workspaceID := range candidates {
		count := byWorkspace[workspaceID]
		if count.matching == count.total {
			exclusive = append(exclusive, workspaceID)
		}
	}
	if len(exclusive) == 1 {
		return exclusive[0]
	}

	var majority []string
	for workspaceID := range candidates {
		count := byWorkspace[workspaceID]
		if count.matching*2 > count.total {
			majority = append(majority, workspaceID)
		}
	}
	if len(majority) == 1 {
		return majority[0]
	}
	return ""
}
