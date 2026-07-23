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
	"syscall"
	"time"
)

const (
	defaultTimeout = 15 * time.Second
	maxOutputBytes = 4 * 1024 * 1024
	termGrace      = 2 * time.Second
	waitDelay      = 4 * time.Second
)

var (
	// ErrDispatchedUnknown means cmd.Start succeeded, but completion could not
	// be proved. Retrying a mutation is unsafe.
	ErrDispatchedUnknown = errors.New("herdr: process started but completion is unknown")
	// ErrNotStarted means no subprocess was created. Retrying is safe.
	ErrNotStarted = errors.New("herdr: process was not started")
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
	bin        string
	socketPath string
	sem        chan struct{}
}

func NewClient(bin, socketPath string) *Client {
	return &Client{bin: bin, socketPath: socketPath, sem: make(chan struct{}, 8)}
}

type Pane struct {
	ID          string `json:"pane_id"`
	TerminalID  string `json:"terminal_id"`
	TabID       string `json:"tab_id"`
	TabLabel    string `json:"tab_label"`
	TabNumber   int    `json:"tab_number"`
	WorkspaceID string `json:"workspace_id"`
	Agent       string `json:"agent"`
	Name        string `json:"name"`
	Status      string `json:"agent_status"`
	Focused     bool   `json:"focused"`
	Cwd         string `json:"cwd"`
	Revision    int    `json:"revision"`
	Scroll      struct {
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
	ID    string `json:"workspace_id"`
	Label string `json:"label"`
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

type AgentInfo struct {
	PaneID  string `json:"pane_id"`
	Agent   string `json:"agent"`
	Name    string `json:"name"`
	Status  string `json:"agent_status"`
	Running bool   `json:"running"`
}

type Inventory struct {
	Panes []Pane
}

func (c *Client) GetInventory(ctx context.Context) (*Inventory, error) {
	var result struct {
		Panes []Pane `json:"panes"`
	}
	if err := c.runResult(ctx, &result, "pane", "list"); err != nil {
		return nil, fmt.Errorf("herdr inventory: %w", err)
	}
	for i := range result.Panes {
		result.Panes[i].Session = result.Panes[i].SessionRaw.Value
	}
	return &Inventory{Panes: result.Panes}, nil
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
		return nil, fmt.Errorf("herdr workspace create: %w", err)
	}
	if result.PaneID == "" {
		return nil, errors.New("herdr workspace create: response has no pane_id")
	}
	return &result, nil
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
		return nil, fmt.Errorf("herdr tab create: %w", err)
	}
	if result.PaneID == "" {
		return nil, errors.New("herdr tab create: response has no pane_id")
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

func (c *Client) ReadPane(ctx context.Context, paneID string, lines int, format string) ([]byte, error) {
	if lines < 1 {
		lines = 1
	}
	if format != "ansi" {
		format = "text"
	}
	return c.runCommand(ctx,
		"pane", "read", paneID,
		"--lines", strconv.Itoa(lines),
		"--source", "recent-unwrapped",
		"--format", format,
	)
}

func (c *Client) SendKeys(ctx context.Context, paneID string, keys []string) error {
	args := []string{"pane", "send-keys", paneID}
	args = append(args, keys...)
	_, err := c.runCommand(ctx, args...)
	return err
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
