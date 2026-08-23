package herdr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultTimeout   = 15 * time.Second
	maxOutputBytes   = 4 * 1024 * 1024
	termGrace        = 2 * time.Second
	waitDelay        = 4 * time.Second
	shiftTabSequence = "\x1b[Z"
)

var (
	// ErrDispatchedUnknown means cmd.Start succeeded, but completion could not
	// be proved. Retrying a mutation is unsafe.
	ErrDispatchedUnknown = errors.New("herdr: process started but completion is unknown")
	// ErrNotStarted means no subprocess was created. Retrying is safe.
	ErrNotStarted = errors.New("herdr: process was not started")
	// ErrCreatedTargetUnknown means Herdr accepted a create command but its
	// response did not identify the new root pane.
	ErrCreatedTargetUnknown = errors.New("herdr: created target response did not identify the root pane")
	// ErrPartiallyApplied means an earlier step of a multi-step mutation already
	// reached the agent before a later step failed. It outranks any
	// safe-to-retry classification derived from the failing step alone,
	// because retrying would duplicate the input that already landed.
	ErrPartiallyApplied = errors.New("herdr: earlier step of the mutation was already applied")
)

// OutcomeError preserves the subprocess dispatch boundary without exposing
// stdout, stderr, user input, or environment data to protocol callers.
type OutcomeError struct {
	Started bool
	Err     error
}

func (e *OutcomeError) Error() string {
	if e == nil || e.Err == nil {
		return "herdr command failed"
	}
	return e.Err.Error()
}

// CLIError is a machine-readable failure returned by the Herdr CLI.
type CLIError struct {
	Code    string
	Message string
}

func (e *CLIError) Error() string {
	if e == nil {
		return "herdr CLI command failed"
	}
	if e.Code == "" {
		return e.Message
	}
	if e.Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}

// refusalCodes are Herdr CLI error codes returned before the command changed
// any state. Herdr validates the request and refuses it, so no input reached
// an agent and no target was mutated: retrying is safe, and the caller may
// retry in place while the condition is transient.
var refusalCodes = map[string]struct{}{
	// The Herdr server is not accepting commands at all.
	"server_not_running": {},
	// The target pane is not an available shell yet. A pane created moments
	// earlier is still running shell startup, so this clears on its own.
	"agent_pane_busy": {},
}

// IsRefused reports whether err carries a Herdr CLI error code that proves the
// command was rejected before it changed anything. It outranks the
// ErrDispatchedUnknown that OutcomeError.Unwrap derives from the subprocess
// boundary alone: the subprocess ran, but Herdr answered with a refusal.
func IsRefused(err error) bool {
	var cliErr *CLIError
	if !errors.As(err, &cliErr) || cliErr == nil {
		return false
	}
	_, refused := refusalCodes[cliErr.Code]
	return refused
}

type cliErrorEnvelope struct {
	ID    string `json:"id"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (e *OutcomeError) Unwrap() error {
	if e == nil {
		return nil
	}
	if e.Started {
		return errors.Join(ErrDispatchedUnknown, e.Err)
	}
	return errors.Join(ErrNotStarted, e.Err)
}

type limitedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	original := len(p)
	remaining := b.limit - b.buf.Len()
	if remaining <= 0 {
		b.truncated = true
		return original, nil
	}
	if len(p) > remaining {
		p = p[:remaining]
		b.truncated = true
	}
	_, err := b.buf.Write(p)
	return original, err
}

func (b *limitedBuffer) Bytes() []byte  { return b.buf.Bytes() }
func (b *limitedBuffer) String() string { return b.buf.String() }

type Client struct {
	bin                    string
	socketPath             string
	sem                    chan struct{}
	api                    *socketAPIClient
	workspaceMoveBlockOnce sync.Once
	workspaceMoveBlock     bool
}

func NewClient(bin, socketPath string) *Client {
	return &Client{
		bin:        bin,
		socketPath: socketPath,
		sem:        make(chan struct{}, 8),
		api:        newSocketAPIClient(socketPath),
	}
}

type Pane struct {
	ID             string `json:"pane_id"`
	TerminalID     string `json:"terminal_id"`
	TabID          string `json:"tab_id"`
	TabLabel       string `json:"tab_label"`
	TabNumber      int    `json:"tab_number"`
	WorkspaceID    string `json:"workspace_id"`
	Agent          string `json:"agent"`
	Name           string `json:"name"`
	Status         string `json:"agent_status"`
	Focused        bool   `json:"focused"`
	Cwd            string `json:"cwd"`
	Revision       int    `json:"revision"`
	StateChangeSeq int64  `json:"state_change_seq"`
	Scroll         struct {
		MaxOffsetFromBottom int `json:"max_offset_from_bottom"`
	} `json:"scroll"`
	ForegroundCwd string `json:"foreground_cwd"`
	Session       string `json:"-"`
	SessionRaw    struct {
		Value string `json:"value"`
		Kind  string `json:"kind"`
	} `json:"agent_session"`
}

type Workspace struct {
	ID          string             `json:"workspace_id"`
	Number      int                `json:"number"`
	Label       string             `json:"label"`
	Focused     bool               `json:"focused"`
	PaneCount   int                `json:"pane_count"`
	TabCount    int                `json:"tab_count"`
	ActiveTabID string             `json:"active_tab_id"`
	AgentStatus string             `json:"agent_status"`
	Cwd         string             `json:"cwd,omitempty"`
	Worktree    *WorkspaceWorktree `json:"worktree,omitempty"`
}

type WorkspaceWorktree struct {
	RepoKey          string `json:"repo_key"`
	RepoName         string `json:"repo_name"`
	RepoRoot         string `json:"repo_root"`
	CheckoutPath     string `json:"checkout_path"`
	IsLinkedWorktree bool   `json:"is_linked_worktree"`
}

type Worktree struct {
	Path            string  `json:"path"`
	Branch          *string `json:"branch"`
	IsBare          bool    `json:"is_bare"`
	IsDetached      bool    `json:"is_detached"`
	IsPrunable      bool    `json:"is_prunable"`
	IsLinked        bool    `json:"is_linked_worktree"`
	Label           string  `json:"label"`
	OpenWorkspaceID *string `json:"open_workspace_id"`
}

type WorktreeSource struct {
	RepoKey            string  `json:"repo_key"`
	RepoName           string  `json:"repo_name"`
	RepoRoot           string  `json:"repo_root"`
	SourceCheckoutPath string  `json:"source_checkout_path"`
	SourceWorkspaceID  *string `json:"source_workspace_id"`
}

type WorktreeListResult struct {
	Source    WorktreeSource `json:"source"`
	Worktrees []Worktree     `json:"worktrees"`
}

type WorktreeMutationResult struct {
	Workspace   Workspace `json:"workspace"`
	Tab         Tab       `json:"tab"`
	RootPane    Pane      `json:"root_pane"`
	Worktree    Worktree  `json:"worktree"`
	AlreadyOpen bool      `json:"already_open,omitempty"`
}

type WorktreeRemoveResult struct {
	WorkspaceID string `json:"workspace_id"`
	Path        string `json:"path"`
	Forced      bool   `json:"forced"`
}

type Tab struct {
	ID          string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
	Label       string `json:"label"`
	Number      int    `json:"number"`
	Cwd         string `json:"cwd"`
}

type CreateResult struct {
	PaneID      string `json:"pane_id"`
	TabID       string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
}

// UnmarshalJSON accepts both the nested Herdr 0.7.5 create responses and the
// flat result returned by older versions and test fixtures.
func (r *CreateResult) UnmarshalJSON(data []byte) error {
	var wire struct {
		PaneID      string    `json:"pane_id"`
		TabID       string    `json:"tab_id"`
		WorkspaceID string    `json:"workspace_id"`
		RootPane    Pane      `json:"root_pane"`
		Tab         Tab       `json:"tab"`
		Workspace   Workspace `json:"workspace"`
		Agent       AgentInfo `json:"agent"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}

	r.PaneID = firstNonEmpty(wire.PaneID, wire.RootPane.ID, wire.Agent.PaneID)
	r.TabID = firstNonEmpty(wire.TabID, wire.RootPane.TabID, wire.Tab.ID)
	r.WorkspaceID = firstNonEmpty(
		wire.WorkspaceID,
		wire.RootPane.WorkspaceID,
		wire.Tab.WorkspaceID,
		wire.Workspace.ID,
	)
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

type AgentInfo struct {
	PaneID  string `json:"pane_id"`
	Agent   string `json:"agent"`
	Name    string `json:"name"`
	Status  string `json:"agent_status"`
	Running bool   `json:"running"`
}

type PaneProcess struct {
	PID     int      `json:"pid"`
	Name    string   `json:"name"`
	Cwd     string   `json:"cwd"`
	Cmdline string   `json:"cmdline"`
	Argv    []string `json:"argv"`
}

type PaneProcessInfo struct {
	PaneID                   string        `json:"pane_id"`
	ShellPID                 int           `json:"shell_pid"`
	ForegroundProcessGroupID int           `json:"foreground_process_group_id"`
	ForegroundProcesses      []PaneProcess `json:"foreground_processes"`
}

type Inventory struct {
	Panes []Pane
}

func (c *Client) GetInventory(ctx context.Context) (*Inventory, error) {
	var result struct {
		Agents []Pane `json:"agents"`
	}
	if err := c.runResult(ctx, &result, "agent", "list"); err == nil {
		for i := range result.Agents {
			result.Agents[i].Session = result.Agents[i].SessionRaw.Value
		}
		return &Inventory{Panes: result.Agents}, nil
	}

	// Herdr versions predating agent-list inventory do not expose a
	// state_change_seq. Keep pane-list compatibility; those relays simply use
	// epoch activity timestamps and the deterministic UI fallback.
	panes, err := c.PaneList(ctx)
	if err != nil {
		return nil, fmt.Errorf("herdr inventory: %w", err)
	}
	return &Inventory{Panes: panes}, nil
}

func (c *Client) PaneList(ctx context.Context) ([]Pane, error) {
	var result struct {
		Panes []Pane `json:"panes"`
	}
	if err := c.runResult(ctx, &result, "pane", "list"); err != nil {
		return nil, fmt.Errorf("herdr pane list: %w", err)
	}
	for index := range result.Panes {
		result.Panes[index].Session = result.Panes[index].SessionRaw.Value
	}
	return result.Panes, nil
}

func (c *Client) WorkspaceList(ctx context.Context) ([]Workspace, error) {
	var result struct {
		Workspaces []Workspace `json:"workspaces"`
	}
	if err := c.runResult(ctx, &result, "workspace", "list"); err != nil {
		return nil, fmt.Errorf("herdr workspace list: %w", err)
	}
	return result.Workspaces, nil
}

func (c *Client) WorkspaceCreate(ctx context.Context, cwd, label string) (*CreateResult, error) {
	var result CreateResult
	if err := c.runResult(ctx, &result,
		"workspace", "create",
		"--cwd", cwd,
		"--label", label,
		"--no-focus",
	); err != nil {
		if !errors.Is(err, ErrNotStarted) && !errors.Is(err, ErrDispatchedUnknown) {
			err = errors.Join(ErrDispatchedUnknown, ErrCreatedTargetUnknown, err)
		}
		return nil, fmt.Errorf("herdr workspace create: %w", err)
	}
	if result.PaneID == "" {
		return nil, fmt.Errorf(
			"herdr workspace create: %w",
			errors.Join(ErrDispatchedUnknown, ErrCreatedTargetUnknown),
		)
	}
	return &result, nil
}

func (c *Client) WorkspaceRename(ctx context.Context, workspaceID, label string) error {
	if _, err := c.runCommand(ctx, "workspace", "rename", workspaceID, label); err != nil {
		return fmt.Errorf("herdr workspace rename: %w", err)
	}
	return nil
}

func (c *Client) WorkspaceMove(ctx context.Context, workspaceID string, insertIndex int) error {
	if err := c.api.workspaceMove(ctx, workspaceID, insertIndex); err != nil {
		return fmt.Errorf("herdr workspace move: %w", err)
	}
	return nil
}

func (c *Client) WorkspaceMoveBlock(
	ctx context.Context,
	workspaceIDs []string,
	beforeWorkspaceID string,
) error {
	if err := c.api.workspaceMoveBlock(ctx, workspaceIDs, beforeWorkspaceID); err != nil {
		return fmt.Errorf("herdr workspace move block: %w", err)
	}
	return nil
}

func (c *Client) WorkspaceClose(ctx context.Context, workspaceID string) error {
	if _, err := c.runCommand(ctx, "workspace", "close", workspaceID); err != nil {
		return fmt.Errorf("herdr workspace close: %w", err)
	}
	return nil
}

func (c *Client) WorktreeList(ctx context.Context, workspaceID string) (*WorktreeListResult, error) {
	var result WorktreeListResult
	if err := c.runResult(ctx, &result, "worktree", "list", "--workspace", workspaceID); err != nil {
		return nil, fmt.Errorf("herdr worktree list: %w", err)
	}
	return &result, nil
}

func (c *Client) WorktreeCreate(
	ctx context.Context,
	workspaceID, branch, base, label string,
) (*WorktreeMutationResult, error) {
	args := []string{"worktree", "create", "--workspace", workspaceID, "--branch", branch, "--no-focus"}
	args = appendOptionalFlag(args, "--base", base)
	args = appendOptionalFlag(args, "--label", label)
	var result WorktreeMutationResult
	if err := c.runResult(ctx, &result, args...); err != nil {
		return nil, fmt.Errorf("herdr worktree create: %w", dispatchedAfterSuccess(err))
	}
	return &result, nil
}

func (c *Client) WorktreeOpen(
	ctx context.Context,
	workspaceID, path, branch, label string,
) (*WorktreeMutationResult, error) {
	args := []string{"worktree", "open", "--workspace", workspaceID, "--no-focus"}
	args = appendOptionalFlag(args, "--path", path)
	args = appendOptionalFlag(args, "--branch", branch)
	args = appendOptionalFlag(args, "--label", label)
	var result WorktreeMutationResult
	if err := c.runResult(ctx, &result, args...); err != nil {
		return nil, fmt.Errorf("herdr worktree open: %w", dispatchedAfterSuccess(err))
	}
	return &result, nil
}

func (c *Client) WorktreeRemove(
	ctx context.Context,
	workspaceID string,
	force bool,
) (*WorktreeRemoveResult, error) {
	args := []string{"worktree", "remove", "--workspace", workspaceID}
	if force {
		args = append(args, "--force")
	}
	var result WorktreeRemoveResult
	if err := c.runResult(ctx, &result, args...); err != nil {
		return nil, fmt.Errorf("herdr worktree remove: %w", dispatchedAfterSuccess(err))
	}
	return &result, nil
}

// dispatchedAfterSuccess classifies runResult failures for mutations. An
// error that carries no dispatch boundary comes from decoding the result
// envelope after the subprocess reported success: the mutation may have
// applied even though its outcome could not be read, so retrying is unsafe.
// Errors already classified at the subprocess boundary keep their marker.
func dispatchedAfterSuccess(err error) error {
	if errors.Is(err, ErrNotStarted) || errors.Is(err, ErrDispatchedUnknown) {
		return err
	}
	return errors.Join(ErrDispatchedUnknown, err)
}

func appendOptionalFlag(args []string, flag, value string) []string {
	if value == "" {
		return args
	}
	return append(args, flag, value)
}

func (c *Client) TabCreate(ctx context.Context, workspaceID, cwd, label string) (*CreateResult, error) {
	var result CreateResult
	if err := c.runResult(ctx, &result,
		"tab", "create",
		"--workspace", workspaceID,
		"--cwd", cwd,
		"--label", label,
		"--no-focus",
	); err != nil {
		if !errors.Is(err, ErrNotStarted) && !errors.Is(err, ErrDispatchedUnknown) {
			err = errors.Join(ErrDispatchedUnknown, ErrCreatedTargetUnknown, err)
		}
		return nil, fmt.Errorf("herdr tab create: %w", err)
	}
	if result.PaneID == "" {
		return nil, fmt.Errorf(
			"herdr tab create: %w",
			errors.Join(ErrDispatchedUnknown, ErrCreatedTargetUnknown),
		)
	}
	return &result, nil
}

func (c *Client) TabList(ctx context.Context) ([]Tab, error) {
	var result struct {
		Tabs []Tab `json:"tabs"`
	}
	if err := c.runResult(ctx, &result, "tab", "list"); err != nil {
		return nil, fmt.Errorf("herdr tab list: %w", err)
	}
	return result.Tabs, nil
}

func (c *Client) TabRename(ctx context.Context, tabID, label string) error {
	if _, err := c.runCommand(ctx, "tab", "rename", tabID, label); err != nil {
		return fmt.Errorf("herdr tab rename: %w", err)
	}
	return nil
}

func (c *Client) TabMove(ctx context.Context, tabID string, insertIndex int) error {
	if err := c.api.tabMove(ctx, tabID, insertIndex); err != nil {
		return fmt.Errorf("herdr tab move: %w", err)
	}
	return nil
}

func (c *Client) PaneRun(ctx context.Context, paneID string, argv []string) error {
	if len(argv) == 0 {
		return errors.New("herdr pane run: empty profile argv")
	}
	command := ShellJoin(argv)
	if _, err := c.runCommand(ctx, "pane", "run", paneID, command); err != nil {
		return fmt.Errorf("herdr pane run: %w", err)
	}
	return nil
}

func (c *Client) AgentGet(ctx context.Context, paneID string) (*AgentInfo, error) {
	var result AgentInfo
	if err := c.runResult(ctx, &result, "agent", "get", paneID); err != nil {
		return nil, fmt.Errorf("herdr agent get: %w", err)
	}
	if result.PaneID == "" {
		result.PaneID = paneID
	}
	return &result, nil
}

func (c *Client) PaneProcessInfo(ctx context.Context, paneID string) (*PaneProcessInfo, error) {
	var result struct {
		ProcessInfo PaneProcessInfo `json:"process_info"`
	}
	if err := c.runResult(ctx, &result, "pane", "process-info", "--pane", paneID); err != nil {
		return nil, fmt.Errorf("herdr pane process info: %w", err)
	}
	return &result.ProcessInfo, nil
}

func (c *Client) ReadPane(ctx context.Context, paneID string, lines int, format string) (PaneRead, error) {
	return c.readPane(ctx, paneID, lines, format, "recent-unwrapped")
}

func (c *Client) ReadPaneRecent(ctx context.Context, paneID string, lines int, format string) (PaneRead, error) {
	return c.readPane(ctx, paneID, lines, format, "recent")
}

func (c *Client) ReadPaneVisible(ctx context.Context, paneID string, lines int, format string) (PaneRead, error) {
	return c.readPane(ctx, paneID, lines, format, "visible")
}

func (c *Client) readPane(ctx context.Context, paneID string, lines int, format, source string) (PaneRead, error) {
	if lines < 1 {
		lines = 1
	}
	if format != "ansi" {
		format = "text"
	}
	if content, err := c.api.readPane(ctx, paneID, lines, format, source); err == nil {
		return content, nil
	}
	// The CLI fallback prints bare text and does not expose the socket API's
	// truncation metadata, so Truncated remains false when this path is used.
	content, err := c.runCommand(ctx,
		"pane", "read", paneID,
		"--lines", strconv.Itoa(lines),
		"--source", source,
		"--format", format,
	)
	if err != nil {
		return PaneRead{}, err
	}
	return PaneRead{Content: content}, nil
}

func (c *Client) ProbePaneVisible(ctx context.Context, paneID string, lines int, format string) ([]byte, error) {
	if lines < 1 {
		lines = 1
	}
	if format != "ansi" {
		format = "text"
	}
	read, err := c.api.readPane(ctx, paneID, lines, format, "visible")
	if err != nil {
		return nil, err
	}
	return read.Content, nil
}

func (c *Client) SupportsRealtimePane(ctx context.Context) bool {
	return c.api.available(ctx)
}

func (c *Client) SupportsWorkspaceMoveBlock() bool {
	c.workspaceMoveBlockOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, err := c.runCommand(ctx, "api", "schema", "--json")
		if err != nil {
			return
		}
		var document struct {
			Schemas struct {
				Request struct {
					OneOf []struct {
						Properties struct {
							Method struct {
								Const string `json:"const"`
							} `json:"method"`
						} `json:"properties"`
					} `json:"oneOf"`
				} `json:"request"`
			} `json:"schemas"`
		}
		if json.Unmarshal(out, &document) != nil {
			return
		}
		for _, request := range document.Schemas.Request.OneOf {
			if request.Properties.Method.Const == "workspace.move_block" {
				c.workspaceMoveBlock = true
				return
			}
		}
	})
	return c.workspaceMoveBlock
}

func (c *Client) Close() error {
	return c.api.close()
}

func (c *Client) SendKeys(ctx context.Context, paneID string, keys []string) error {
	if len(keys) == 1 && strings.EqualFold(keys[0], "shift+tab") {
		return c.SendText(ctx, paneID, shiftTabSequence)
	}
	args := make([]string, 0, 3+len(keys))
	args = append(args, "pane", "send-keys", paneID)
	for _, key := range keys {
		args = append(args, normalizeTerminalKey(key))
	}
	_, err := c.runCommand(ctx, args...)
	return err
}

func normalizeTerminalKey(key string) string {
	if len(key) != len("ctrl+x") || !strings.EqualFold(key[:len("ctrl+")], "ctrl+") {
		return key
	}
	letter := key[len(key)-1]
	if letter >= 'A' && letter <= 'Z' {
		return "ctrl+" + string(letter+'a'-'A')
	}
	return key
}

func (c *Client) SendText(ctx context.Context, paneID, text string) error {
	_, err := c.runCommand(ctx, "pane", "send-text", paneID, text)
	return err
}

func (c *Client) Prompt(ctx context.Context, paneID, text string) error {
	_, err := c.runCommand(ctx, "agent", "prompt", paneID, text)
	return err
}

func (c *Client) StopPane(ctx context.Context, paneID string) error {
	_, err := c.runCommand(ctx, "pane", "close", paneID)
	return err
}

func (c *Client) RenameAgent(ctx context.Context, paneID, name string) error {
	_, err := c.runCommand(ctx, "agent", "rename", paneID, name)
	return err
}

// RenamePane is retained as a compatibility alias for callers being migrated.
func (c *Client) RenamePane(ctx context.Context, paneID, name string) error {
	return c.RenameAgent(ctx, paneID, name)
}

func (c *Client) StartAgent(ctx context.Context, name, kind, paneID string, timeoutMs int) (string, error) {
	var result CreateResult
	if err := c.runResult(ctx, &result,
		"agent", "start", name,
		"--kind", kind,
		"--pane", paneID,
		"--timeout", strconv.Itoa(timeoutMs),
	); err != nil {
		return "", fmt.Errorf("herdr agent start: %w", err)
	}
	if result.PaneID == "" {
		result.PaneID = paneID
	}
	if result.PaneID == "" {
		return "", errors.New("herdr agent start: response has no pane_id")
	}
	return result.PaneID, nil
}

func (c *Client) IntegrationStatus(ctx context.Context) ([]byte, error) {
	return c.runCommand(ctx, "integration", "status")
}

func (c *Client) runResult(ctx context.Context, result any, args ...string) error {
	out, err := c.runCommand(ctx, args...)
	if err != nil {
		return err
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(out, &envelope); err != nil {
		return fmt.Errorf("malformed JSON envelope: %w", err)
	}
	if len(envelope.Result) == 0 || bytes.Equal(envelope.Result, []byte("null")) {
		return errors.New("response has no result envelope")
	}
	if err := json.Unmarshal(envelope.Result, result); err != nil {
		return fmt.Errorf("malformed result envelope: %w", err)
	}
	return nil
}

// run is retained for package-level process-boundary tests. Production callers
// carry their own absolute deadline and use runCommand.
func (c *Client) run(parent context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	return c.runCommand(ctx, args...)
}

func (c *Client) runCommand(parent context.Context, args ...string) ([]byte, error) {
	ctx := parent
	cancel := func() {}
	if _, ok := parent.Deadline(); !ok {
		ctx, cancel = context.WithTimeout(parent, defaultTimeout)
	}
	defer cancel()

	if err := ctx.Err(); err != nil {
		return nil, &OutcomeError{Started: false, Err: err}
	}

	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return nil, &OutcomeError{Started: false, Err: ctx.Err()}
	}

	cmd := exec.Command(c.bin, args...)
	cmd.Env = append(cmd.Environ(), "HERDR_SOCKET_PATH="+c.socketPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Process-group termination owns cancellation. WaitDelay is the final
	// backstop for inherited stdout/stderr descriptors held by a descendant
	// that escaped the group before cancellation.
	cmd.WaitDelay = waitDelay

	stdout := &limitedBuffer{limit: maxOutputBytes}
	stderr := &limitedBuffer{limit: maxOutputBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		return nil, &OutcomeError{Started: false, Err: fmt.Errorf("start: %w", err)}
	}

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	select {
	case err := <-waitCh:
		return commandResult(stdout, stderr, err)
	case <-ctx.Done():
		terminateProcessGroup(cmd.Process.Pid, waitCh)
		return nil, &OutcomeError{Started: true, Err: ctx.Err()}
	}
}

func commandResult(stdout, stderr *limitedBuffer, waitErr error) ([]byte, error) {
	if waitErr == nil {
		return append([]byte(nil), stdout.Bytes()...), nil
	}
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) {
		return nil, &OutcomeError{Started: true, Err: waitErr}
	}
	diagnostic := strings.TrimSpace(stderr.String())
	if !stderr.truncated && diagnostic != "" {
		var envelope cliErrorEnvelope
		if err := json.Unmarshal([]byte(diagnostic), &envelope); err == nil &&
			envelope.Error != nil && envelope.Error.Code != "" {
			return nil, &OutcomeError{
				Started: true,
				Err: &CLIError{
					Code:    envelope.Error.Code,
					Message: envelope.Error.Message,
				},
			}
		}
	}
	if diagnostic == "" {
		diagnostic = exitErr.Error()
	}
	if stderr.truncated {
		diagnostic += " (truncated)"
	}
	if len(diagnostic) > 500 {
		diagnostic = diagnostic[:500] + "..."
	}
	return nil, &OutcomeError{Started: true, Err: fmt.Errorf("command failed: %s", diagnostic)}
}

func terminateProcessGroup(pgid int, waitCh <-chan error) {
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	timer := time.NewTimer(termGrace)
	defer timer.Stop()
	leaderDone := false
	select {
	case <-waitCh:
		leaderDone = true
		if !GroupAlive(pgid) {
			return
		}
		<-timer.C
	case <-timer.C:
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)

	if !leaderDone {
		select {
		case <-waitCh:
		case <-time.After(2 * time.Second):
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for GroupAlive(pgid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
}

// ShellJoin is equivalent to shlex.join for the POSIX shell grammar used by
// `herdr pane run`. Only configured profile argv is accepted by PaneRun.
func ShellJoin(argv []string) string {
	quoted := make([]string, len(argv))
	for i, value := range argv {
		quoted[i] = shellQuote(value)
	}
	return strings.Join(quoted, " ")
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	const safe = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_@%+=:,./-"
	if strings.IndexFunc(value, func(r rune) bool { return !strings.ContainsRune(safe, r) }) == -1 {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

// GroupAlive is exported only for process-boundary tests and diagnostics.
func GroupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
