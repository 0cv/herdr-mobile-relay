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
	// agentStartProcessTimeoutMS caps the --timeout handed to a single
	// `herdr agent start`. The effective value is the caller's remaining
	// budget, so a retry can never ask Herdr to outlive the request.
	agentStartProcessTimeoutMS = 30000
	// agentStartResponseReserve keeps the startup work ahead of the command
	// deadline, so a failure is classified precisely instead of surfacing as a
	// context timeout the phone cannot act on.
	agentStartResponseReserve = 5 * time.Second
	customAgentPollInterval   = 250 * time.Millisecond
	// agentStartRetryInitial and agentStartRetryMax bound the wait between
	// start attempts while Herdr refuses the freshly created pane: its shell
	// has not reached a prompt yet. Herdr answers agent_pane_busy in about a
	// millisecond, before its own --timeout applies, and every attempt forks a
	// subprocess through an 8-slot semaphore shared with every other pane
	// command. The interval therefore grows instead of polling flat out.
	agentStartRetryInitial = 150 * time.Millisecond
	agentStartRetryMax     = 1500 * time.Millisecond
)

var agentNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

type StartRequest struct {
	ProfileID   string
	WorkspaceID string
	Name        string
	Cwd         string
	Prompt      string
}

type StartResult struct {
	PaneID      string `json:"pane_id"`
	Name        string `json:"name"`
	Cwd         string `json:"cwd"`
	WorkspaceID string `json:"workspace_id,omitempty"`
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
	cwd, err := l.ResolveCwd(request.Cwd)
	if err != nil {
		return profiles.Profile{}, request, err
	}
	request.Cwd = cwd
	return profile, request, nil
}

func (l *Lifecycle) Start(ctx context.Context, profile profiles.Profile, request StartRequest) (StartResult, error) {
	if existing := l.reconcileExisting(ctx, profile.ID, request); existing != "" {
		l.profiles.Remember(existing, profile.ID)
		return StartResult{PaneID: existing, Name: request.Name, Cwd: request.Cwd, WorkspaceID: request.WorkspaceID}, nil
	}

	deadline, ok := ctx.Deadline()
	if !ok {
		return StartResult{}, errors.New("agent start requires an absolute deadline")
	}
	startupDeadline := deadline.Add(-agentStartResponseReserve)
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
	workspaceID := request.WorkspaceID
	if workspaceID != "" && !workspaceExists(workspaces, workspaceID) {
		return StartResult{}, errors.New("workspace is unavailable")
	}
	if workspaceID == "" {
		workspaceID = SelectWorkspaceForCwd(request.Cwd, inventory.Panes, workspaces, l.home)
	}

	target, err := l.createTarget(startupCtx, workspaceID, request.Name, request.Cwd)
	if err != nil {
		return StartResult{}, err
	}

	startErr := l.startInTarget(startupCtx, profile, request.Name, target.PaneID)
	if startErr != nil {
		// The target stays open. Herdr created it, so closing it would destroy
		// the workspace the user asked for and leave nothing to retry into. An
		// uncertain dispatch may also have left an agent running in it, and
		// the phone is told to review that agent before retrying.
		return StartResult{PaneID: target.PaneID, Name: request.Name, Cwd: request.Cwd, WorkspaceID: target.WorkspaceID}, startErr
	}

	l.profiles.Remember(target.PaneID, profile.ID)
	return StartResult{PaneID: target.PaneID, Name: request.Name, Cwd: request.Cwd, WorkspaceID: target.WorkspaceID}, nil
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
		return l.startKindAgent(ctx, profile.Kind, name, paneID)
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

// startKindAgent retries while Herdr refuses the target pane. A pane created
// milliseconds earlier is still running shell startup, and Herdr rejects the
// start with agent_pane_busy before its own --timeout window opens, so the
// timeout the relay passes cannot cover it. The refusal proves nothing ran,
// which makes the retry safe.
func (l *Lifecycle) startKindAgent(ctx context.Context, kind, name, paneID string) error {
	delay := agentStartRetryInitial
	for {
		_, err := l.herdr.StartAgent(ctx, name, kind, paneID, remainingTimeoutMS(ctx))
		if err == nil || !herdr.IsRefused(err) {
			return err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			// The refusal, not the context error: it is the actual cause and
			// it keeps the safe-to-retry classification the phone acts on.
			return err
		case <-timer.C:
		}
		delay = min(delay*2, agentStartRetryMax)
	}
}

// remainingTimeoutMS is the budget the caller still holds. Handing Herdr a
// fixed timeout would let one attempt outlive the request, so a retry could
// never run; it would also outlive the phone's own deadline.
func remainingTimeoutMS(ctx context.Context) int {
	deadline, ok := ctx.Deadline()
	if !ok {
		return agentStartProcessTimeoutMS
	}
	remaining := time.Until(deadline).Milliseconds()
	if remaining <= 0 {
		return 0
	}
	return int(min(remaining, agentStartProcessTimeoutMS))
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
		if request.WorkspaceID != "" && pane.WorkspaceID != request.WorkspaceID {
			continue
		}
		if l.profiles.ResolvePane(pane.ID, pane.Agent) == profileID {
			return pane.ID
		}
	}
	return ""
}

func workspaceExists(workspaces []herdr.Workspace, workspaceID string) bool {
	for _, workspace := range workspaces {
		if workspace.ID == workspaceID {
			return true
		}
	}
	return false
}

func (l *Lifecycle) ResolveCwd(raw string) (string, error) {
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
	resolvedHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		return "", errors.New("home directory is unavailable")
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", errors.New("cwd is not an accessible directory inside the home directory")
	}
	relative, err := filepath.Rel(resolvedHome, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("cwd must be inside the home directory")
	}
	if relative == "." {
		return "", errors.New("cwd must be a project directory below the home directory")
	}
	root, err := os.OpenRoot(resolvedHome)
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
	return resolved, nil
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
