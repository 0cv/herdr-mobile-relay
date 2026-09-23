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
	agentStartRetryInitial = 50 * time.Millisecond
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
	Identity    LaunchIdentity `json:"-"`
	PaneID      string         `json:"pane_id"`
	Name        string         `json:"name"`
	Cwd         string         `json:"cwd"`
	WorkspaceID string         `json:"workspace_id,omitempty"`
}

type Lifecycle struct {
	state        *State
	herdr        *herdr.Client
	profiles     *profiles.Resolver
	home         string
	waitForRetry func(context.Context, time.Duration) bool
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

func (l *Lifecycle) Start(ctx context.Context, profile profiles.Profile, request StartRequest) (result StartResult, resultErr error) {
	expectedTerminal := ""
	if l.state != nil {
		snapshot := l.state.beginLaunch()
		defer func() {
			if resultErr == nil && result.PaneID != "" {
				result.Identity = l.state.finishLaunch(snapshot, result.PaneID)
				if expectedTerminal != "" {
					if result.Identity.TerminalID != "" && result.Identity.TerminalID != expectedTerminal {
						result.Identity.Valid = false
					}
					result.Identity.TerminalID = expectedTerminal
				}
			}
		}()
	}
	if existing := l.reconcileExisting(ctx, profile.ID, request); existing.PaneID != "" {
		expectedTerminal = existing.TerminalID
		return StartResult{PaneID: existing.PaneID, Name: request.Name, Cwd: request.Cwd, WorkspaceID: request.WorkspaceID}, nil
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

	result = StartResult{PaneID: target.PaneID, Name: request.Name, Cwd: request.Cwd, WorkspaceID: target.WorkspaceID}
	panes, err := l.herdr.PaneList(startupCtx)
	if err != nil {
		return result, fmt.Errorf("verify new terminal: %w", err)
	}
	var ownership profiles.PaneIdentity
	for _, pane := range panes {
		if pane.ID == target.PaneID {
			ownership = profiles.PaneIdentity{PaneID: pane.ID, TerminalID: pane.TerminalID, TabID: pane.TabID, WorkspaceID: pane.WorkspaceID}
			break
		}
	}
	expectedTerminal = ownership.TerminalID
	if ownership.TerminalID == "" {
		return result, errors.New("cannot verify the new terminal; agent was not started")
	}
	if err := l.profiles.BeginLaunchOwnership(ownership, profile.ID); err != nil {
		return result, err
	}
	startErr := l.startInTarget(startupCtx, profile, request.Name, target.PaneID)
	if startErr != nil {
		// The target stays open. Herdr created it, so closing it would destroy
		// the workspace the user asked for and leave nothing to retry into. An
		// uncertain dispatch may also have left an agent running in it, and
		// the phone is told to review that agent before retrying.
		return result, startErr
	}

	if err := l.profiles.RememberVerified(ownership, profile.ID); err != nil {
		return result, partiallyApplied("agent started but profile ownership could not be saved", err)
	}
	return result, nil
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
			if err := l.herdr.RenameAgent(ctx, paneID, name); err != nil {
				return partiallyApplied("custom agent was already started", err)
			}
			return nil
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
	wait := l.waitForRetry
	if wait == nil {
		wait = waitForAgentRetry
	}
	return retryAgentStart(ctx, func() error {
		_, err := l.herdr.StartAgent(ctx, name, kind, paneID, remainingTimeoutMS(ctx))
		return err
	}, wait)
}

func retryAgentStart(ctx context.Context, start func() error, wait func(context.Context, time.Duration) bool) error {
	delay := agentStartRetryInitial
	for {
		err := start()
		if err == nil || !herdr.IsTransientRefused(err) {
			return err
		}
		if !wait(ctx, delay) {
			return err
		}
		delay = min(delay*2, agentStartRetryMax)
	}
}

func waitForAgentRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
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

func (l *Lifecycle) reconcileExisting(ctx context.Context, profileID string, request StartRequest) profiles.PaneIdentity {
	inventory, err := l.herdr.GetInventory(ctx)
	if err != nil {
		return profiles.PaneIdentity{}
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
		target := profiles.PaneIdentity{PaneID: pane.ID, TerminalID: pane.TerminalID, TabID: pane.TabID, WorkspaceID: pane.WorkspaceID}
		if l.profiles.OwnsTarget(target, profileID) {
			return target
		}
	}
	return profiles.PaneIdentity{}
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
