package panesize

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

const (
	MinColumns = 40
	MaxColumns = 240
	LeaseTTL   = 30 * time.Second

	sweepInterval  = time.Second
	commandTimeout = 3 * time.Second
)

var (
	ErrClosed             = errors.New("Pane size leasing is shut down")
	ErrInvalidColumns     = errors.New("Columns must be between 40 and 240")
	ErrInvalidLease       = errors.New("Pane and lease owner are required")
	ErrLeaseOwnerGone     = errors.New("Pane size lease owner is disconnected")
	ErrProcessUnavailable = errors.New("Pane foreground process information is unavailable")
	ErrTTYUnavailable     = errors.New("Pane foreground process does not have a TTY")
	ErrSizeUnavailable    = errors.New("Pane terminal size is unavailable")
	ErrResizeFailed       = errors.New("Pane terminal columns could not be changed")
	ErrUnsupportedOS      = errors.New("Pane size leasing is unsupported on this platform")
)

type ProcessInfoProvider interface {
	PaneProcessInfo(context.Context, string) (*herdr.PaneProcessInfo, error)
}

type commandRunner interface {
	Output(context.Context, string, ...string) ([]byte, error)
}

type execCommandRunner struct{}

func (execCommandRunner) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

type Lease struct {
	Columns   int
	ExpiresAt time.Time
}

type paneState struct {
	tty             string
	baselineRows    int
	baselineColumns int
	appliedColumns  int
	resizedAt       time.Time
	leases          map[string]Lease
}

type Manager struct {
	mu       sync.Mutex
	provider ProcessInfoProvider
	runner   commandRunner
	goos     string
	ttl      time.Duration
	now      func() time.Time
	logger   *slog.Logger
	panes    map[string]*paneState
	closed   bool
}

func NewManager(provider ProcessInfoProvider, logger *slog.Logger) *Manager {
	return newManager(provider, execCommandRunner{}, runtime.GOOS, LeaseTTL, time.Now, logger)
}

func newManager(
	provider ProcessInfoProvider,
	runner commandRunner,
	goos string,
	ttl time.Duration,
	now func() time.Time,
	logger *slog.Logger,
) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		provider: provider,
		runner:   runner,
		goos:     goos,
		ttl:      ttl,
		now:      now,
		logger:   logger,
		panes:    make(map[string]*paneState),
	}
}

func (m *Manager) Acquire(
	ctx context.Context,
	clientID, paneID string,
	columns int,
) (int, error) {
	if clientID == "" || paneID == "" {
		return 0, ErrInvalidLease
	}
	if columns < MinColumns || columns > MaxColumns {
		return 0, ErrInvalidColumns
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return 0, ErrClosed
	}
	if ctx.Err() != nil {
		return 0, ErrLeaseOwnerGone
	}

	now := m.now()
	state := m.panes[paneID]
	newState := state == nil
	if newState {
		var err error
		state, err = m.resolvePane(ctx, paneID)
		if err != nil {
			return 0, err
		}
	} else {
		m.removeExpired(state, now)
		if size, err := m.readSize(ctx, state.tty); err != nil {
			return 0, err
		} else if size.columns != state.appliedColumns {
			// A local terminal resize while the lease is active becomes the new
			// restore point. Rows are observed but are never changed by a lease.
			state.baselineRows = size.rows
			state.baselineColumns = size.columns
		}
	}
	if ctx.Err() != nil {
		return 0, ErrLeaseOwnerGone
	}

	previous, hadPrevious := state.leases[clientID]
	state.leases[clientID] = Lease{Columns: columns, ExpiresAt: now.Add(m.ttl)}
	target, _ := minimumColumns(state.leases)
	if err := m.setColumns(ctx, state.tty, target); err != nil {
		if hadPrevious {
			state.leases[clientID] = previous
		} else {
			delete(state.leases, clientID)
		}
		if newState {
			m.panes[paneID] = state
		}
		return 0, err
	}
	if target != state.appliedColumns {
		state.resizedAt = now
	}
	state.appliedColumns = target
	if newState {
		m.panes[paneID] = state
	}
	return target, nil
}

// ActiveColumns reports the narrowest unexpired lease for a pane.
func (m *Manager) ActiveColumns(paneID string) (int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return 0, false
	}
	state := m.panes[paneID]
	if state == nil {
		return 0, false
	}
	now := m.now()
	minimum := 0
	for _, lease := range state.leases {
		if !lease.ExpiresAt.After(now) {
			continue
		}
		if minimum == 0 || lease.Columns < minimum {
			minimum = lease.Columns
		}
	}
	return minimum, minimum != 0
}

// ResizedWithin reports whether a lease actually changed the pane's terminal
// width within the given window. Renewals that keep the same columns do not
// count: they do not signal the application, so it does not re-render.
func (m *Manager) ResizedWithin(paneID string, window time.Duration) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return false
	}
	state := m.panes[paneID]
	if state == nil || state.resizedAt.IsZero() {
		return false
	}
	return m.now().Sub(state.resizedAt) < window
}

// ActiveRows reports the unchanged terminal height for an actively leased pane.
func (m *Manager) ActiveRows(paneID string) (int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return 0, false
	}
	state := m.panes[paneID]
	if state == nil {
		return 0, false
	}
	now := m.now()
	for _, lease := range state.leases {
		if lease.ExpiresAt.After(now) {
			return state.baselineRows, true
		}
	}
	return 0, false
}

func (m *Manager) Release(ctx context.Context, clientID, paneID string) error {
	if clientID == "" || paneID == "" {
		return ErrInvalidLease
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	state := m.panes[paneID]
	if state == nil {
		return nil
	}
	if _, exists := state.leases[clientID]; !exists {
		return nil
	}
	delete(state.leases, clientID)
	return m.reconcile(ctx, paneID, state)
}

func (m *Manager) ReleaseClient(ctx context.Context, clientID string) error {
	if clientID == "" {
		return ErrInvalidLease
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}

	var result error
	for paneID, state := range m.panes {
		_, owned := state.leases[clientID]
		if owned {
			delete(state.leases, clientID)
		}
		_, active := minimumColumns(state.leases)
		if !owned && active {
			continue
		}
		if err := m.reconcile(ctx, paneID, state); err != nil {
			result = errors.Join(result, fmt.Errorf("pane %s: %w", paneID, err))
		}
	}
	return result
}

func (m *Manager) SweepExpired(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}

	now := m.now()
	var result error
	for paneID, state := range m.panes {
		removed := m.removeExpired(state, now)
		target, active := minimumColumns(state.leases)
		if !removed && active && state.appliedColumns == target {
			continue
		}
		if err := m.reconcile(ctx, paneID, state); err != nil {
			result = errors.Join(result, fmt.Errorf("pane %s: %w", paneID, err))
		}
	}
	return result
}

func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweepCtx, cancel := context.WithTimeout(ctx, commandTimeout)
			err := m.SweepExpired(sweepCtx)
			cancel()
			if err != nil {
				m.logger.Warn("pane size lease expiry sweep failed", "error", err)
			}
		}
	}
}

func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true

	var result error
	for paneID, state := range m.panes {
		clear(state.leases)
		if err := m.restore(ctx, paneID, state); err != nil {
			result = errors.Join(result, fmt.Errorf("pane %s: %w", paneID, err))
		}
	}
	return result
}

func (m *Manager) resolvePane(ctx context.Context, paneID string) (*paneState, error) {
	if m.provider == nil {
		return nil, ErrProcessUnavailable
	}
	info, err := m.provider.PaneProcessInfo(ctx, paneID)
	if err != nil {
		return nil, ErrProcessUnavailable
	}
	pid, err := foregroundPID(info, paneID)
	if err != nil {
		return nil, err
	}
	output, err := m.runner.Output(ctx, "ps", "-o", "tty=", "-p", strconv.Itoa(pid))
	if err != nil {
		return nil, ErrTTYUnavailable
	}
	tty, err := ttyPath(output)
	if err != nil {
		return nil, err
	}
	size, err := m.readSize(ctx, tty)
	if err != nil {
		return nil, err
	}
	return &paneState{
		tty:             tty,
		baselineRows:    size.rows,
		baselineColumns: size.columns,
		appliedColumns:  size.columns,
		leases:          make(map[string]Lease),
	}, nil
}

func foregroundPID(info *herdr.PaneProcessInfo, paneID string) (int, error) {
	if info == nil || info.PaneID == "" || info.PaneID != paneID ||
		info.ForegroundProcessGroupID <= 0 || len(info.ForegroundProcesses) == 0 {
		return 0, ErrProcessUnavailable
	}
	for _, process := range info.ForegroundProcesses {
		if process.PID == info.ForegroundProcessGroupID {
			return process.PID, nil
		}
	}
	for _, process := range info.ForegroundProcesses {
		if process.PID > 0 {
			return process.PID, nil
		}
	}
	return 0, ErrProcessUnavailable
}

type terminalSize struct {
	rows    int
	columns int
}

func (m *Manager) readSize(ctx context.Context, tty string) (terminalSize, error) {
	flag, err := sttyDeviceFlag(m.goos)
	if err != nil {
		return terminalSize{}, err
	}
	output, err := m.runner.Output(ctx, "stty", flag, tty, "size")
	if err != nil {
		return terminalSize{}, ErrSizeUnavailable
	}
	fields := strings.Fields(string(output))
	if len(fields) != 2 {
		return terminalSize{}, ErrSizeUnavailable
	}
	rows, rowErr := strconv.Atoi(fields[0])
	columns, columnErr := strconv.Atoi(fields[1])
	if rowErr != nil || columnErr != nil || rows < 1 || columns < 1 {
		return terminalSize{}, ErrSizeUnavailable
	}
	return terminalSize{rows: rows, columns: columns}, nil
}

func (m *Manager) setColumns(ctx context.Context, tty string, columns int) error {
	flag, err := sttyDeviceFlag(m.goos)
	if err != nil {
		return err
	}
	if _, err := m.runner.Output(
		ctx,
		"stty",
		flag,
		tty,
		"cols",
		strconv.Itoa(columns),
	); err != nil {
		return ErrResizeFailed
	}
	return nil
}

func sttyDeviceFlag(goos string) (string, error) {
	switch goos {
	case "linux":
		return "-F", nil
	case "darwin":
		return "-f", nil
	default:
		return "", ErrUnsupportedOS
	}
}

func ttyPath(output []byte) (string, error) {
	fields := strings.Fields(string(output))
	if len(fields) != 1 || fields[0] == "?" || fields[0] == "??" || fields[0] == "-" {
		return "", ErrTTYUnavailable
	}
	tty := strings.TrimPrefix(fields[0], "/dev/")
	if tty == "" || filepath.IsAbs(tty) {
		return "", ErrTTYUnavailable
	}
	clean := filepath.Clean(tty)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", ErrTTYUnavailable
	}
	return filepath.Join("/dev", clean), nil
}

func minimumColumns(leases map[string]Lease) (int, bool) {
	minimum := 0
	for _, lease := range leases {
		if minimum == 0 || lease.Columns < minimum {
			minimum = lease.Columns
		}
	}
	return minimum, minimum != 0
}

func (m *Manager) removeExpired(state *paneState, now time.Time) bool {
	removed := false
	for clientID, lease := range state.leases {
		if lease.ExpiresAt.After(now) {
			continue
		}
		delete(state.leases, clientID)
		removed = true
	}
	return removed
}

func (m *Manager) reconcile(ctx context.Context, paneID string, state *paneState) error {
	target, active := minimumColumns(state.leases)
	if !active {
		return m.restore(ctx, paneID, state)
	}
	if target == state.appliedColumns {
		return nil
	}
	if err := m.setColumns(ctx, state.tty, target); err != nil {
		return err
	}
	state.resizedAt = m.now()
	state.appliedColumns = target
	return nil
}

func (m *Manager) restore(ctx context.Context, paneID string, state *paneState) error {
	if err := m.setColumns(ctx, state.tty, state.baselineColumns); err != nil {
		return err
	}
	state.appliedColumns = state.baselineColumns
	delete(m.panes, paneID)
	return nil
}
