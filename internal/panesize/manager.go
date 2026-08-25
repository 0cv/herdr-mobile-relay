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
	MinRows    = 10
	MaxRows    = 120
	// LeaseTTL must ride out hidden-tab timer clamping: desktop Safari
	// reports an occluded window as hidden and degrades its timers to a
	// measured ~60-65s cadence within two minutes, so a 30s TTL lapsed the
	// lease — and resized the shared pane — during every longer glance away,
	// even though renewals were still arriving. Twice the measured clamp
	// keeps a renewing-but-hidden page leased; a frozen or vanished client
	// gives the pane back within two minutes instead of half a minute, and a
	// closing client is still released immediately through ReleaseClient.
	LeaseTTL = 120 * time.Second
	// ReleaseGrace keeps a released width in place for a moment. Leaving a
	// terminal on the phone and stepping back into it is a few seconds apart,
	// and restoring the pane in between resizes it twice: the agent re-renders
	// on both SIGWINCHs, so its output looks stalled for as long as it repaints.
	// The pane keeping the phone's width this much longer is the cheaper trade.
	ReleaseGrace = 10 * time.Second

	sweepInterval  = time.Second
	commandTimeout = 3 * time.Second
)

var (
	ErrClosed             = errors.New("Pane size leasing is shut down")
	ErrInvalidColumns     = errors.New("Columns must be between 40 and 240")
	ErrInvalidRows        = errors.New("Rows must be between 10 and 120")
	ErrInvalidLease       = errors.New("Pane and lease owner are required")
	ErrLeaseOwnerGone     = errors.New("Pane size lease owner is disconnected")
	ErrProcessUnavailable = errors.New("Pane foreground process information is unavailable")
	ErrTTYUnavailable     = errors.New("Pane foreground process does not have a TTY")
	ErrSizeUnavailable    = errors.New("Pane terminal size is unavailable")
	ErrResizeFailed       = errors.New("Pane terminal size could not be changed")
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
	Columns int
	// Rows is zero when the client leases only the width: the pane keeps
	// its own height and old clients stay valid without sending rows.
	Rows      int
	ExpiresAt time.Time
}

type paneState struct {
	tty             string
	baselineRows    int
	baselineColumns int
	appliedRows     int
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
	grace    time.Duration
	now      func() time.Time
	logger   *slog.Logger
	panes    map[string]*paneState
	closed   bool
}

func NewManager(provider ProcessInfoProvider, logger *slog.Logger) *Manager {
	return newManager(provider, execCommandRunner{}, runtime.GOOS, LeaseTTL, ReleaseGrace, time.Now, logger)
}

func newManager(
	provider ProcessInfoProvider,
	runner commandRunner,
	goos string,
	ttl time.Duration,
	grace time.Duration,
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
		grace:    grace,
		now:      now,
		logger:   logger,
		panes:    make(map[string]*paneState),
	}
}

func (m *Manager) Acquire(
	ctx context.Context,
	clientID, paneID string,
	columns, rows int,
) (int, int, error) {
	if clientID == "" || paneID == "" {
		return 0, 0, ErrInvalidLease
	}
	if columns < MinColumns || columns > MaxColumns {
		return 0, 0, ErrInvalidColumns
	}
	if rows != 0 && (rows < MinRows || rows > MaxRows) {
		return 0, 0, ErrInvalidRows
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return 0, 0, ErrClosed
	}
	if ctx.Err() != nil {
		return 0, 0, ErrLeaseOwnerGone
	}

	now := m.now()
	state := m.panes[paneID]
	newState := state == nil
	var current terminalSize
	if newState {
		var err error
		state, err = m.resolvePane(ctx, paneID)
		if err != nil {
			return 0, 0, err
		}
		current = terminalSize{rows: state.appliedRows, columns: state.appliedColumns}
	} else {
		m.removeExpired(state, now)
		var err error
		current, err = m.readSize(ctx, state.tty)
		if err != nil {
			return 0, 0, err
		}
		// A local terminal resize while the lease is active becomes the new
		// restore point, per dimension.
		if current.columns != state.appliedColumns {
			state.baselineColumns = current.columns
		}
		if current.rows != state.appliedRows {
			state.baselineRows = current.rows
		}
	}
	if ctx.Err() != nil {
		return 0, 0, ErrLeaseOwnerGone
	}

	previous, hadPrevious := state.leases[clientID]
	state.leases[clientID] = Lease{Columns: columns, Rows: rows, ExpiresAt: now.Add(m.ttl)}
	targetColumns, _ := minimumColumns(state.leases)
	targetRows := minimumRows(state.leases)
	constrainedRows := targetRows > 0
	if !constrainedRows {
		targetRows = state.baselineRows
	}
	// A width-only pane never gets its height touched: rows reach stty only
	// while a row lease constrains them or a lapsed one must be undone.
	sttyRows := targetRows
	if !constrainedRows && targetRows == state.appliedRows {
		sttyRows = 0
	}
	// A renewal extends the lease only. Calling stty with dimensions the TTY
	// already has reaches its resize syscall every ten seconds; some terminal
	// stacks repaint on that request even though the dimensions are unchanged.
	resizeNeeded := targetColumns != current.columns
	if sttyRows > 0 && targetRows != current.rows {
		resizeNeeded = true
	}
	if resizeNeeded {
		if err := m.setSize(ctx, state.tty, targetColumns, sttyRows); err != nil {
			if hadPrevious {
				state.leases[clientID] = previous
			} else {
				delete(state.leases, clientID)
			}
			if newState {
				m.panes[paneID] = state
			}
			return 0, 0, err
		}
		state.resizedAt = now
	}
	state.appliedColumns = targetColumns
	state.appliedRows = targetRows
	if newState {
		m.panes[paneID] = state
	}
	return targetColumns, targetRows, nil
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

// ActiveRows reports the effective terminal height for an actively leased
// pane: the smallest unexpired row lease, or the pane's baseline height when
// no client leases rows.
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
	active := false
	minimum := 0
	for _, lease := range state.leases {
		if !lease.ExpiresAt.After(now) {
			continue
		}
		active = true
		if lease.Rows > 0 && (minimum == 0 || lease.Rows < minimum) {
			minimum = lease.Rows
		}
	}
	if !active {
		return 0, false
	}
	if minimum == 0 {
		return state.baselineRows, true
	}
	return minimum, true
}

// Release lapses the lease instead of restoring the pane immediately. A phone
// that returns to the terminal within ReleaseGrace re-acquires the width it
// already has, so nothing is resized and the agent does not re-render; the
// expiry sweep restores the pane once the grace elapses. A disconnecting
// client keeps the immediate path through ReleaseClient.
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
	lease, exists := state.leases[clientID]
	if !exists {
		return nil
	}
	now := m.now()
	if lapse := now.Add(m.grace); lease.ExpiresAt.After(lapse) {
		lease.ExpiresAt = lapse
		state.leases[clientID] = lease
	}
	if lease.ExpiresAt.After(now) {
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
		targetRows := minimumRows(state.leases)
		if targetRows == 0 {
			targetRows = state.baselineRows
		}
		if !removed && active && state.appliedColumns == target && state.appliedRows == targetRows {
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
		appliedRows:     size.rows,
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

func (m *Manager) setSize(ctx context.Context, tty string, columns, rows int) error {
	flag, err := sttyDeviceFlag(m.goos)
	if err != nil {
		return err
	}
	args := []string{flag, tty, "cols", strconv.Itoa(columns)}
	if rows > 0 {
		args = append(args, "rows", strconv.Itoa(rows))
	}
	if _, err := m.runner.Output(ctx, "stty", args...); err != nil {
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

// minimumRows reports the smallest row constraint across leases; zero when
// every lease is width-only.
func minimumRows(leases map[string]Lease) int {
	minimum := 0
	for _, lease := range leases {
		if lease.Rows <= 0 {
			continue
		}
		if minimum == 0 || lease.Rows < minimum {
			minimum = lease.Rows
		}
	}
	return minimum
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
	targetRows := minimumRows(state.leases)
	constrainedRows := targetRows > 0
	if !constrainedRows {
		targetRows = state.baselineRows
	}
	if target == state.appliedColumns && targetRows == state.appliedRows {
		return nil
	}
	sttyRows := targetRows
	if !constrainedRows && targetRows == state.appliedRows {
		sttyRows = 0
	}
	if err := m.setSize(ctx, state.tty, target, sttyRows); err != nil {
		return err
	}
	state.resizedAt = m.now()
	state.appliedColumns = target
	state.appliedRows = targetRows
	return nil
}

func (m *Manager) restore(ctx context.Context, paneID string, state *paneState) error {
	sttyRows := state.baselineRows
	if state.appliedRows == state.baselineRows {
		// The height was never leased away; leave the tty's rows alone.
		sttyRows = 0
	}
	if err := m.setSize(ctx, state.tty, state.baselineColumns, sttyRows); err != nil {
		return err
	}
	state.appliedColumns = state.baselineColumns
	state.appliedRows = state.baselineRows
	delete(m.panes, paneID)
	return nil
}
