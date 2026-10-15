// Package panesize resizes a shared pane's PTY while a phone that opted in to
// Resize Session has it open, and gives the pane its size back afterwards.
//
// Herdr 0.9.3 (socket protocol 22) has no operation that resizes a pane's
// grid and PTY together, so the relay writes the PTY window size directly.
// Herdr's own grid stays at the layout size meanwhile, which garbles the
// desktop and SSH views of that pane for as long as the lease is held. That
// cost is inherent and disclosed to the user before they opt in.
//
// Every write is bound to the terminal the lease was acquired on; see
// openValidated for the identity boundary and docs/pane-size-leasing.md for
// the design and its residual risks.
package panesize

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"syscall"
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
	// measured ~60-65s cadence within two minutes. Twice the measured clamp
	// keeps a renewing-but-hidden page leased; a frozen or vanished client
	// gives the pane back within two minutes, and a closing client is still
	// released immediately through ReleaseClient.
	LeaseTTL = 120 * time.Second
	// ReleaseGrace keeps a released size in place for a moment. Leaving a
	// terminal on the phone and stepping back into it is a few seconds apart,
	// and restoring the pane in between resizes it twice. Turning Resize
	// Session off skips the grace (an immediate release).
	ReleaseGrace = 10 * time.Second

	// CleanupAttempts and CleanupBudget bound how long a pane nobody leases
	// any more is retried: at most five restore attempts, all inside thirty
	// seconds from the moment the last lease ended. The time spent in Herdr
	// lookups, the TTY lookup, open and ioctl counts against the budget.
	CleanupAttempts = 5
	CleanupBudget   = 30 * time.Second

	cleanupFirstBackoff = time.Second
	cleanupMaxBackoff   = 10 * time.Second
	sweepInterval       = time.Second
	// operationTimeout bounds one mutation attempt end to end, so one slow
	// pane cannot hold up another pane's lease or cleanup indefinitely.
	operationTimeout = 3 * time.Second
)

var (
	ErrUnavailable         = errors.New("Resize Session is unavailable on this relay")
	ErrClosed              = errors.New("Resize Session is shutting down")
	ErrInvalidColumns      = errors.New("Columns must be between 40 and 240")
	ErrInvalidRows         = errors.New("Rows must be between 10 and 120")
	ErrInvalidLease        = errors.New("Pane and lease owner are required")
	ErrLeaseOwnerGone      = errors.New("Pane size lease owner is disconnected")
	ErrIdentityUnavailable = errors.New("Pane terminal identity is unavailable")
	ErrTerminalGone        = errors.New("Pane terminal was closed or replaced")
	ErrTerminalUnverified  = errors.New("Pane terminal could not be verified; try again")
	ErrTTYUnavailable      = errors.New("Pane shell does not have a terminal")
	ErrSizeUnavailable     = errors.New("Pane terminal size is unavailable")
	ErrResizeFailed        = errors.New("Pane terminal size could not be changed")
	ErrUnsupportedOS       = errors.New("Resize Session is unsupported on this platform")
)

var publicErrors = []error{
	ErrUnavailable, ErrClosed, ErrInvalidColumns, ErrInvalidRows, ErrInvalidLease,
	ErrLeaseOwnerGone, ErrIdentityUnavailable, ErrTerminalGone, ErrTerminalUnverified,
	ErrSizeUnavailable, ErrResizeFailed, ErrUnsupportedOS,
}

// PublicMessage is the phone-facing text for a manager error. Lookup detail
// (socket errors, device numbers, process ids) stays in the relay log.
func PublicMessage(err error) string {
	for _, known := range publicErrors {
		if errors.Is(err, known) {
			return known.Error()
		}
	}
	return "Resize Session failed"
}

// terminalGone is a confirmed outcome: the pane, its terminal, or its TTY is
// no longer the one the lease was acquired on. Nothing is written and the
// pane's state is dropped.
type terminalGone struct{ reason string }

func (e *terminalGone) Error() string { return ErrTerminalGone.Error() + ": " + e.reason }
func (e *terminalGone) Unwrap() error { return ErrTerminalGone }

func gone(reason string) error { return &terminalGone{reason: reason} }

// unverified is an unknown outcome: a lookup or device operation failed for
// a reason that does not prove the terminal changed. Nothing is written; a
// cleanup retries within its budget.
func unverified(step string, err error) error {
	if err == nil {
		return fmt.Errorf("%w: %s", ErrTerminalUnverified, step)
	}
	return fmt.Errorf("%w: %s: %v", ErrTerminalUnverified, step, err)
}

func identityUnavailable(step string, err error) error {
	if err == nil {
		return fmt.Errorf("%w: %s", ErrIdentityUnavailable, step)
	}
	return fmt.Errorf("%w: %s: %v", ErrIdentityUnavailable, step, err)
}

type Lease struct {
	Columns int
	// Rows is zero when the client leases only the width: the pane keeps
	// its own height, which is never written with a different value.
	Rows      int
	ExpiresAt time.Time
}

// terminalIdentity is captured at acquisition and re-validated before every
// write. shellPID is zero only when Herdr reported no shell and the TTY was
// found through the foreground process group leader instead.
type terminalIdentity struct {
	terminalID string
	shellPID   int
	ttyPath    string
	rdev       uint64
}

type size struct {
	columns int
	rows    int
}

// cleanupState is a pending restore of a pane nobody leases any more. Its
// deadline is fixed when the last lease ends; nothing but a new acquisition
// (which supersedes it) or the end of the cleanup changes it.
type cleanupState struct {
	generation uint64
	attempts   int
	next       time.Time
	deadline   time.Time
	lastErr    error
}

type paneState struct {
	// lock serializes terminal I/O for this pane (open, validate, ioctl) and
	// the decision that precedes it. It is a channel so waiting is bounded by
	// a context. Field access is guarded by Manager.mu, never by lock.
	lock      chan struct{}
	resolved  bool
	identity  terminalIdentity
	baseline  size
	applied   size
	resizedAt time.Time
	leases    map[string]Lease
	// generation fences cleanup: superseding a pending cleanup increments it,
	// so a cleanup attempt planned earlier can never write over, or delete,
	// the newer lease.
	generation uint64
	cleanup    *cleanupState
}

func (s *paneState) acquire(ctx context.Context) error {
	select {
	case s.lock <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *paneState) tryAcquire() bool {
	select {
	case s.lock <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *paneState) release() { <-s.lock }

type Manager struct {
	mu        sync.Mutex
	provider  Provider
	resolver  TTYResolver
	ops       TerminalOps
	available bool
	opTimeout time.Duration
	ttl       time.Duration
	grace     time.Duration
	now       func() time.Time
	logger    *slog.Logger
	panes     map[string]*paneState
	closed    bool
	onResize  func(string)
}

// NewManager returns the production manager: fresh Herdr lookups through
// provider, `ps` for the shell's TTY, and direct winsize ioctls. It is
// available only on darwin and linux.
func NewManager(provider Provider, logger *slog.Logger) *Manager {
	m := newManager(provider, psTTYResolver{}, ioctlTerminalOps{}, LeaseTTL, ReleaseGrace, time.Now, logger)
	m.available = platformSupported && provider != nil
	return m
}

// NewManagerWith builds an available manager from injected parts, so callers
// can be tested deterministically without Herdr, processes or real TTYs.
func NewManagerWith(
	provider Provider,
	resolver TTYResolver,
	ops TerminalOps,
	now func() time.Time,
	logger *slog.Logger,
) *Manager {
	return newManager(provider, resolver, ops, LeaseTTL, ReleaseGrace, now, logger)
}

func newManager(
	provider Provider,
	resolver TTYResolver,
	ops TerminalOps,
	ttl time.Duration,
	grace time.Duration,
	now func() time.Time,
	logger *slog.Logger,
) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		provider:  provider,
		resolver:  resolver,
		ops:       ops,
		available: true,
		opTimeout: operationTimeout,
		ttl:       ttl,
		grace:     grace,
		now:       now,
		logger:    logger,
		panes:     make(map[string]*paneState),
	}
}

func (m *Manager) SetOnResize(callback func(string)) {
	m.mu.Lock()
	m.onResize = callback
	m.mu.Unlock()
}

func (m *Manager) notifyResize(paneID string) {
	m.mu.Lock()
	callback := m.onResize
	m.mu.Unlock()
	if callback != nil {
		callback(paneID)
	}
}

// Available reports whether this relay can resize panes at all. The relay
// advertises Resize Session only when it is true.
func (m *Manager) Available() bool {
	return m != nil && m.available
}

// Acquire creates or renews clientID's lease on paneID and applies the
// narrowest leased size across clients. Every call re-validates the pane's
// terminal and reads its current size through the validated descriptor, so a
// local resize during the lease becomes that dimension's restore point.
func (m *Manager) Acquire(
	ctx context.Context,
	clientID, paneID string,
	columns, rows int,
) (int, int, error) {
	if !m.Available() {
		return 0, 0, ErrUnavailable
	}
	if clientID == "" || paneID == "" {
		return 0, 0, ErrInvalidLease
	}
	if columns < MinColumns || columns > MaxColumns {
		return 0, 0, ErrInvalidColumns
	}
	if rows != 0 && (rows < MinRows || rows > MaxRows) {
		return 0, 0, ErrInvalidRows
	}
	if ctx.Err() != nil {
		return 0, 0, ErrLeaseOwnerGone
	}
	opCtx, cancel := context.WithTimeout(ctx, m.opTimeout)
	defer cancel()
	state, created, err := m.lockPane(opCtx, paneID)
	if err != nil {
		switch {
		case errors.Is(err, ErrClosed):
			return 0, 0, ErrClosed
		case ctx.Err() != nil:
			return 0, 0, ErrLeaseOwnerGone
		default:
			return 0, 0, unverified("pane is busy", err)
		}
	}
	defer state.release()
	forget := func() {
		m.mu.Lock()
		m.dropLocked(paneID, state)
		m.mu.Unlock()
	}

	var identity terminalIdentity
	if created {
		identity, err = m.lookupIdentity(opCtx, paneID)
		if err != nil {
			forget()
			return 0, 0, err
		}
	} else {
		m.mu.Lock()
		identity = state.identity
		m.mu.Unlock()
	}
	terminal, err := m.openValidated(opCtx, paneID, &identity, created)
	if err != nil {
		if created {
			forget()
		} else if errors.Is(err, ErrTerminalGone) {
			m.dropGone(paneID, state, err)
		}
		return 0, 0, err
	}
	defer terminal.Close()
	current, err := terminal.Size()
	if err != nil {
		if created {
			forget()
		}
		return 0, 0, fmt.Errorf("%w: %v", ErrSizeUnavailable, err)
	}
	if ctx.Err() != nil {
		if created {
			forget()
		}
		return 0, 0, ErrLeaseOwnerGone
	}

	now := m.now()
	lease := Lease{Columns: columns, Rows: rows, ExpiresAt: now.Add(m.ttl)}
	m.mu.Lock()
	// Cancellation must be checked while holding the same bookkeeping lock
	// as ReleaseClient's disconnect scan. Otherwise a scan between the earlier
	// context check and this insertion can miss a newly orphaned lease.
	if ctx.Err() != nil || opCtx.Err() != nil {
		if created {
			m.dropLocked(paneID, state)
		}
		m.mu.Unlock()
		if ctx.Err() != nil {
			return 0, 0, ErrLeaseOwnerGone
		}
		return 0, 0, unverified("acquisition deadline", opCtx.Err())
	}
	if created {
		state.identity = identity
		state.resolved = true
		state.baseline = sizeOf(current)
		state.applied = sizeOf(current)
	} else {
		m.removeExpiredLocked(state, now)
		if state.applied != sizeOf(current) {
			defer m.notifyResize(paneID)
		}
		state.observeLocked(current)
	}
	previous, hadPrevious := state.leases[clientID]
	state.leases[clientID] = lease
	want := state.leaseTargetLocked(current)
	m.mu.Unlock()

	if next, changed := resized(current, want); changed {
		if err := terminal.SetSize(next); err != nil {
			m.mu.Lock()
			// Undo only this request's lease: a concurrent disconnect may
			// already have removed it, and must not see it come back.
			if state.leases[clientID] == lease {
				if hadPrevious {
					state.leases[clientID] = previous
				} else {
					delete(state.leases, clientID)
				}
			}
			if created {
				m.dropLocked(paneID, state)
			}
			m.mu.Unlock()
			return 0, 0, fmt.Errorf("%w: %v", ErrResizeFailed, err)
		}
		m.notifyResize(paneID)
	}

	m.mu.Lock()
	state.commitLocked(current, want, now)
	m.removeExpiredLocked(state, m.now())
	if state.cleanup != nil && len(state.leases) > 0 {
		// Only a still-live acquisition supersedes a pending restore. If a
		// disconnect or immediate release removed the last lease during I/O,
		// keep its original cleanup deadline and generation. Otherwise the
		// baseline stays and a new generation fences the superseded cleanup.
		state.cleanup = nil
		state.generation++
	}
	m.mu.Unlock()
	return want.columns, want.rows, nil
}

// Release ends clientID's lease on paneID. Without immediate the lease lapses
// after ReleaseGrace, so stepping straight back into the pane re-leases the
// size it already has instead of resizing it twice. Release always succeeds
// for the caller: a restore that cannot complete now is retried by the
// bounded cleanup, never by the client.
func (m *Manager) Release(ctx context.Context, clientID, paneID string, immediate bool) error {
	if clientID == "" || paneID == "" {
		return ErrInvalidLease
	}
	m.mu.Lock()
	state := m.panes[paneID]
	if m.closed || state == nil {
		m.mu.Unlock()
		return nil
	}
	now := m.now()
	m.removeExpiredLocked(state, now)
	lease, exists := state.leases[clientID]
	if !exists {
		m.mu.Unlock()
		return nil
	}
	if !immediate {
		if lapse := now.Add(m.grace); lease.ExpiresAt.After(lapse) {
			lease.ExpiresAt = lapse
			state.leases[clientID] = lease
		}
		if lease.ExpiresAt.After(now) {
			m.mu.Unlock()
			return nil
		}
	}
	delete(state.leases, clientID)
	if len(state.leases) == 0 {
		ended := now
		if !immediate {
			ended = lease.ExpiresAt
		}
		m.beginCleanupLocked(state, ended)
	}
	m.mu.Unlock()
	m.settle(ctx, paneID, state)
	return nil
}

// ReleaseClient immediately ends every lease a disconnecting client holds.
func (m *Manager) ReleaseClient(ctx context.Context, clientID string) error {
	if clientID == "" {
		return ErrInvalidLease
	}
	type affected struct {
		paneID string
		state  *paneState
	}
	var panes []affected
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	now := m.now()
	for paneID, state := range m.panes {
		if _, owned := state.leases[clientID]; owned {
			m.removeExpiredLocked(state, now)
			delete(state.leases, clientID)
			if len(state.leases) == 0 {
				m.beginCleanupLocked(state, now)
			}
			panes = append(panes, affected{paneID: paneID, state: state})
		}
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, pane := range panes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.settle(ctx, pane.paneID, pane.state)
		}()
	}
	wg.Wait()
	return nil
}

// SweepExpired lapses expired leases, reconciles panes whose lease set
// changed, and makes due cleanup attempts. Each pane is handled on its own
// goroutine with its own timeout; a pane busy with another operation is left
// for the next sweep.
func (m *Manager) SweepExpired(ctx context.Context) {
	type work struct {
		paneID     string
		state      *paneState
		generation uint64
		cleanup    bool
	}
	var due []work
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	now := m.now()
	for paneID, state := range m.panes {
		if !state.resolved {
			continue
		}
		removed := m.removeExpiredLocked(state, now)
		if len(state.leases) == 0 {
			generation := m.beginCleanupLocked(state, now)
			if !now.Before(state.cleanup.next) {
				due = append(due, work{paneID: paneID, state: state, generation: generation, cleanup: true})
			}
			continue
		}
		// A still-leased pane is reconciled once when a lease lapses. A
		// failure is not retried here: the remaining clients' renewals
		// re-apply the size, and their TTL bounds the pane's lifetime.
		if removed && state.reconcileNeededLocked() {
			due = append(due, work{paneID: paneID, state: state, generation: state.generation})
		}
	}
	m.mu.Unlock()

	var wg sync.WaitGroup
	for _, item := range due {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !item.state.tryAcquire() {
				return
			}
			defer item.state.release()
			opCtx, cancel := context.WithTimeout(ctx, m.opTimeout)
			defer cancel()
			if item.cleanup {
				m.attemptCleanup(opCtx, item.paneID, item.state, item.generation)
				return
			}
			_ = m.reconcileActive(opCtx, item.paneID, item.state)
		}()
	}
	wg.Wait()
}

func (m *Manager) Run(ctx context.Context) {
	if !m.Available() {
		return
	}
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.SweepExpired(ctx)
		}
	}
}

// Shutdown stops accepting leases and makes one restore attempt for every
// tracked pane, all bounded by ctx. Panes that could not be restored are
// logged and returned as an error; success is never claimed for them.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	panes := make(map[string]*paneState, len(m.panes))
	for paneID, state := range m.panes {
		panes[paneID] = state
	}
	m.mu.Unlock()

	var (
		wg         sync.WaitGroup
		resultMu   sync.Mutex
		unresolved []string
	)
	for paneID, state := range panes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.shutdownPane(ctx, paneID, state); err != nil {
				resultMu.Lock()
				unresolved = append(unresolved, paneID+" ("+err.Error()+")")
				resultMu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(unresolved) == 0 {
		return nil
	}
	slices.Sort(unresolved)
	m.logger.Warn("pane sizes were not restored at shutdown; resize these panes locally",
		"panes", strings.Join(unresolved, "; "))
	return fmt.Errorf("pane sizes not restored: %s", strings.Join(unresolved, "; "))
}

func (m *Manager) shutdownPane(ctx context.Context, paneID string, state *paneState) error {
	if err := state.acquire(ctx); err != nil {
		return fmt.Errorf("busy until the shutdown deadline: %w", err)
	}
	defer state.release()
	m.mu.Lock()
	if m.panes[paneID] != state || !state.resolved {
		m.dropLocked(paneID, state)
		m.mu.Unlock()
		return nil
	}
	clear(state.leases)
	state.cleanup = nil
	m.mu.Unlock()
	opCtx, cancel := context.WithTimeout(ctx, m.opTimeout)
	defer cancel()
	err := m.restoreOnce(opCtx, paneID, state)
	m.mu.Lock()
	m.dropLocked(paneID, state)
	m.mu.Unlock()
	if errors.Is(err, ErrTerminalGone) {
		m.logger.Info("pane size lease dropped: the pane's terminal was closed or replaced",
			"pane_id", paneID, "reason", err.Error())
		return nil
	}
	return err
}

// ReadGeometry reports the leased geometry a pane read should use, but only
// for a client that itself holds an active lease on that pane: everyone else
// reads the pane at Herdr's own size.
func (m *Manager) ReadGeometry(clientID, paneID string) (int, int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return 0, 0, false
	}
	state := m.panes[paneID]
	if state == nil {
		return 0, 0, false
	}
	now := m.now()
	own, owned := state.leases[clientID]
	if !owned || !own.ExpiresAt.After(now) {
		return 0, 0, false
	}
	columns, rows := 0, 0
	for _, lease := range state.leases {
		if !lease.ExpiresAt.After(now) {
			continue
		}
		if columns == 0 || lease.Columns < columns {
			columns = lease.Columns
		}
		if lease.Rows > 0 && (rows == 0 || lease.Rows < rows) {
			rows = lease.Rows
		}
	}
	if rows == 0 {
		rows = state.baseline.rows
	}
	return columns, rows, true
}

// ResizedWithin reports whether a lease actually changed the pane's terminal
// size within the given window. Renewals that keep the same size do not
// count: they send no SIGWINCH, so the application does not re-render.
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

// lockPane returns the pane's current state with its I/O lock held. A missing
// pane gets an unresolved placeholder, so concurrent first acquisitions of
// one pane serialize on a single state.
func (m *Manager) lockPane(ctx context.Context, paneID string) (*paneState, bool, error) {
	for {
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return nil, false, ErrClosed
		}
		state := m.panes[paneID]
		if state == nil {
			state = &paneState{lock: make(chan struct{}, 1), leases: make(map[string]Lease)}
			state.lock <- struct{}{} // a fresh channel never blocks
			m.panes[paneID] = state
			m.mu.Unlock()
			return state, true, nil
		}
		m.mu.Unlock()
		if err := state.acquire(ctx); err != nil {
			return nil, false, err
		}
		m.mu.Lock()
		current := !m.closed && m.panes[paneID] == state
		m.mu.Unlock()
		if current {
			return state, false, nil
		}
		state.release()
	}
}

// lookupIdentity finds the terminal an acquisition binds to: Herdr's
// terminal_id from a fresh pane.get (the inventory does not cover plain shell
// panes), the shell's pid from a fresh pane.process_info, and that process's
// TTY. The device number is added by openValidated once the TTY is open.
func (m *Manager) lookupIdentity(ctx context.Context, paneID string) (terminalIdentity, error) {
	if m.provider == nil || m.resolver == nil || m.ops == nil {
		return terminalIdentity{}, ErrUnavailable
	}
	pane, err := m.provider.PaneGet(ctx, paneID)
	if err != nil {
		if herdr.IsPaneNotFound(err) {
			return terminalIdentity{}, gone("pane closed")
		}
		return terminalIdentity{}, identityUnavailable("pane.get", err)
	}
	if pane == nil || pane.TerminalID == "" || (pane.ID != "" && pane.ID != paneID) {
		return terminalIdentity{}, identityUnavailable("Herdr reported no terminal_id", nil)
	}
	info, err := m.provider.PaneProcessInfoFresh(ctx, paneID)
	if err != nil {
		if herdr.IsPaneNotFound(err) {
			return terminalIdentity{}, gone("pane closed")
		}
		return terminalIdentity{}, identityUnavailable("pane.process_info", err)
	}
	if info == nil || (info.PaneID != "" && info.PaneID != paneID) {
		return terminalIdentity{}, identityUnavailable("pane.process_info returned another pane", nil)
	}
	shellPID := info.ShellPID
	pid := shellPID
	if pid <= 0 {
		shellPID = 0
		var ok bool
		if pid, ok = foregroundPID(info); !ok {
			return terminalIdentity{}, identityUnavailable("Herdr reported no shell or foreground process", nil)
		}
	}
	tty, err := m.resolver.TTYPath(ctx, pid)
	if err != nil {
		return terminalIdentity{}, identityUnavailable("shell TTY", err)
	}
	return terminalIdentity{terminalID: pane.TerminalID, shellPID: shellPID, ttyPath: tty}, nil
}

// openValidated is the mutation boundary. It (a) opens the cached TTY path
// read-only without becoming its controlling process, (b) checks with fstat
// that the held descriptor is a character device with the recorded device
// number, and only then (c) re-validates the pane with fresh lookups: the same
// Herdr terminal_id, the same shell pid, and that pid's TTY is still the
// cached path. The caller performs its ioctls on the returned descriptor and
// closes it; a successful check is never reused for a later write.
//
// The order is the point. A pseudo-terminal is not returned to the allocator
// while any descriptor on it is open: Linux frees a devpts index only when the
// last master and slave references close, and XNU frees a ptmx minor only
// once both its master and slave sides are closed. Opening first pins the
// device. If the shell recorded at acquisition is afterwards still alive on
// this TTY, the TTY was never released between our open and the check, so the
// held descriptor is that shell's terminal and cannot be a reused PTY that now
// belongs to another pane. Checking first and opening second would leave
// exactly that window open.
//
// With capture (acquisition), the device number is recorded instead of
// compared.
func (m *Manager) openValidated(ctx context.Context, paneID string, identity *terminalIdentity, capture bool) (Terminal, error) {
	terminal, err := m.ops.Open(identity.ttyPath)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENXIO) {
			return nil, gone("terminal device no longer exists")
		}
		return nil, unverified("open terminal device", err)
	}
	rdev, character, err := terminal.Device()
	switch {
	case err != nil:
		_ = terminal.Close()
		return nil, unverified("fstat terminal device", err)
	case !character:
		_ = terminal.Close()
		return nil, gone("terminal path is not a character device")
	case capture:
		identity.rdev = rdev
	case rdev != identity.rdev:
		_ = terminal.Close()
		return nil, gone("terminal device number changed")
	}
	if err := m.validateIdentity(ctx, paneID, *identity); err != nil {
		_ = terminal.Close()
		return nil, err
	}
	if ctx.Err() != nil {
		_ = terminal.Close()
		return nil, unverified("validation deadline", ctx.Err())
	}
	return terminal, nil
}

func (m *Manager) validateIdentity(ctx context.Context, paneID string, identity terminalIdentity) error {
	pane, err := m.provider.PaneGet(ctx, paneID)
	if err != nil {
		if herdr.IsPaneNotFound(err) {
			return gone("pane closed")
		}
		return unverified("pane.get", err)
	}
	if pane == nil || pane.TerminalID == "" {
		return unverified("Herdr reported no terminal_id", nil)
	}
	if pane.TerminalID != identity.terminalID {
		return gone("pane terminal was replaced")
	}
	info, err := m.provider.PaneProcessInfoFresh(ctx, paneID)
	if err != nil {
		if herdr.IsPaneNotFound(err) {
			return gone("pane closed")
		}
		return unverified("pane.process_info", err)
	}
	if info == nil || (info.PaneID != "" && info.PaneID != paneID) {
		return unverified("pane.process_info returned another pane", nil)
	}
	pid := identity.shellPID
	if pid > 0 {
		if info.ShellPID != pid {
			return gone("pane shell process changed")
		}
	} else {
		if info.ShellPID > 0 {
			return gone("pane shell process changed")
		}
		var ok bool
		if pid, ok = foregroundPID(info); !ok {
			return unverified("Herdr reported no foreground process", nil)
		}
	}
	tty, err := m.resolver.TTYPath(ctx, pid)
	if err != nil {
		return unverified("shell TTY", err)
	}
	if tty != identity.ttyPath {
		return gone("pane shell is on another terminal")
	}
	return nil
}

// settle brings one pane in line with its remaining leases after a lease
// ended: a still-leased pane is reconciled once, and a pane nobody leases
// starts its cleanup with a first attempt now. A pane busy past the timeout
// is left for the sweep.
func (m *Manager) settle(ctx context.Context, paneID string, state *paneState) {
	opCtx, cancel := context.WithTimeout(ctx, m.opTimeout)
	defer cancel()
	if err := state.acquire(opCtx); err != nil {
		return
	}
	defer state.release()
	m.mu.Lock()
	if m.closed || m.panes[paneID] != state || !state.resolved {
		m.mu.Unlock()
		return
	}
	now := m.now()
	m.removeExpiredLocked(state, now)
	if len(state.leases) == 0 {
		generation := m.beginCleanupLocked(state, now)
		m.mu.Unlock()
		m.attemptCleanup(opCtx, paneID, state, generation)
		return
	}
	needed := state.reconcileNeededLocked()
	m.mu.Unlock()
	if needed {
		_ = m.reconcileActive(opCtx, paneID, state)
	}
}

// reconcileActive applies the current lease target to a still-leased pane.
// The caller holds the pane's lock.
func (m *Manager) reconcileActive(ctx context.Context, paneID string, state *paneState) error {
	m.mu.Lock()
	if m.panes[paneID] != state || len(state.leases) == 0 {
		m.mu.Unlock()
		return nil
	}
	identity := state.identity
	m.mu.Unlock()
	terminal, err := m.openValidated(ctx, paneID, &identity, false)
	if err != nil {
		if errors.Is(err, ErrTerminalGone) {
			m.dropGone(paneID, state, err)
		}
		return err
	}
	defer terminal.Close()
	current, err := terminal.Size()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSizeUnavailable, err)
	}
	now := m.now()
	m.mu.Lock()
	if m.panes[paneID] != state || len(state.leases) == 0 {
		m.mu.Unlock()
		return nil
	}
	if state.applied != sizeOf(current) {
		defer m.notifyResize(paneID)
	}
	state.observeLocked(current)
	want := state.leaseTargetLocked(current)
	m.mu.Unlock()
	if next, changed := resized(current, want); changed {
		if err := terminal.SetSize(next); err != nil {
			return fmt.Errorf("%w: %v", ErrResizeFailed, err)
		}
		m.notifyResize(paneID)
	}
	m.mu.Lock()
	if m.panes[paneID] == state {
		state.commitLocked(current, want, now)
	}
	m.mu.Unlock()
	return nil
}

// beginCleanupLocked starts the restore budget for a pane whose last lease
// ended. An existing cleanup keeps its attempts and deadline.
func (m *Manager) beginCleanupLocked(state *paneState, now time.Time) uint64 {
	if state.cleanup == nil {
		state.cleanup = &cleanupState{
			generation: state.generation,
			next:       now,
			deadline:   now.Add(CleanupBudget),
		}
	}
	return state.cleanup.generation
}

// attemptCleanup makes one due restore attempt. The caller holds the pane's
// lock. A cleanup superseded by a new acquisition (different generation) is
// fenced off both before and after the I/O.
func (m *Manager) attemptCleanup(ctx context.Context, paneID string, state *paneState, generation uint64) {
	m.mu.Lock()
	cleanup := state.cleanup
	if m.panes[paneID] != state || cleanup == nil || cleanup.generation != generation ||
		state.generation != generation || len(state.leases) > 0 {
		m.mu.Unlock()
		return
	}
	now := m.now()
	if now.Before(cleanup.next) {
		m.mu.Unlock()
		return
	}
	if cleanup.attempts >= CleanupAttempts || !now.Before(cleanup.deadline) {
		m.exhaustLocked(paneID, state, cleanup)
		m.mu.Unlock()
		return
	}
	cleanup.attempts++
	remaining := cleanup.deadline.Sub(now)
	m.mu.Unlock()

	attemptCtx, cancel := context.WithTimeout(ctx, min(m.opTimeout, remaining))
	err := m.restoreOnce(attemptCtx, paneID, state)
	cancel()

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.panes[paneID] != state || state.cleanup != cleanup || state.generation != generation {
		return
	}
	switch {
	case err == nil:
		m.dropLocked(paneID, state)
	case errors.Is(err, ErrTerminalGone):
		m.dropLocked(paneID, state)
		m.logger.Info("pane size lease dropped: the pane's terminal was closed or replaced",
			"pane_id", paneID, "reason", err.Error())
	default:
		cleanup.lastErr = err
		next := m.now().Add(cleanupBackoff(cleanup.attempts))
		if cleanup.attempts >= CleanupAttempts || !next.Before(cleanup.deadline) {
			m.exhaustLocked(paneID, state, cleanup)
			return
		}
		cleanup.next = next
	}
}

// restoreOnce gives the pane its pre-lease size back through a validated
// descriptor, per dimension: a dimension whose current value is no longer the
// one the lease applied was changed locally meanwhile and is kept; any other
// is set back to its baseline. Value equality is a best-effort heuristic. A
// local change back to the applied value, or a change between this read and
// the write, cannot be told apart. The caller holds the pane's lock.
func (m *Manager) restoreOnce(ctx context.Context, paneID string, state *paneState) error {
	m.mu.Lock()
	identity, applied, baseline := state.identity, state.applied, state.baseline
	m.mu.Unlock()
	if applied == baseline {
		return nil // nothing the lease changed is left to undo
	}
	terminal, err := m.openValidated(ctx, paneID, &identity, false)
	if err != nil {
		return err
	}
	defer terminal.Close()
	current, err := terminal.Size()
	if err != nil {
		return unverified("read terminal size", err)
	}
	want := sizeOf(current)
	if int(current.Columns) == applied.columns {
		want.columns = baseline.columns
	}
	if int(current.Rows) == applied.rows {
		want.rows = baseline.rows
	}
	if next, changed := resized(current, want); changed {
		if err := terminal.SetSize(next); err != nil {
			return unverified("write terminal size", err)
		}
		m.notifyResize(paneID)
	}
	return nil
}

func (m *Manager) exhaustLocked(paneID string, state *paneState, cleanup *cleanupState) {
	m.dropLocked(paneID, state)
	attrs := []any{"pane_id", paneID, "attempts", cleanup.attempts}
	if cleanup.lastErr != nil {
		attrs = append(attrs, "error", cleanup.lastErr.Error())
	}
	m.logger.Warn("pane size was not restored after Resize Session; resize the pane locally to recover", attrs...)
}

func (m *Manager) dropGone(paneID string, state *paneState, err error) {
	m.mu.Lock()
	dropped := m.panes[paneID] == state
	m.dropLocked(paneID, state)
	m.mu.Unlock()
	if dropped {
		m.logger.Info("pane size lease dropped: the pane's terminal was closed or replaced",
			"pane_id", paneID, "reason", err.Error())
	}
}

func (m *Manager) dropLocked(paneID string, state *paneState) {
	if m.panes[paneID] == state {
		delete(m.panes, paneID)
	}
	state.cleanup = nil
}

func (m *Manager) removeExpiredLocked(state *paneState, now time.Time) bool {
	removed := false
	var lastExpiry time.Time
	for clientID, lease := range state.leases {
		if lease.ExpiresAt.After(now) {
			continue
		}
		delete(state.leases, clientID)
		removed = true
		if lease.ExpiresAt.After(lastExpiry) {
			lastExpiry = lease.ExpiresAt
		}
	}
	if removed && len(state.leases) == 0 {
		// A delayed sweep must not give an expired lease a fresh cleanup
		// budget. Start at the last lease's expiry, not the observation time.
		m.beginCleanupLocked(state, lastExpiry)
	}
	return removed
}

// observeLocked adopts a local resize made while the pane is leased as that
// dimension's new restore point.
func (s *paneState) observeLocked(current Winsize) {
	if columns := int(current.Columns); columns != s.applied.columns {
		s.baseline.columns = columns
		s.applied.columns = columns
	}
	if rows := int(current.Rows); rows != s.applied.rows {
		s.baseline.rows = rows
		s.applied.rows = rows
	}
}

// leaseTargetLocked is the size an actively leased pane should have: the
// narrowest leased width and, when any client leases rows, the shortest
// leased height. Without a row lease the height is the baseline when a lapsed
// row lease must be undone, and otherwise whatever the terminal has now.
func (s *paneState) leaseTargetLocked(current Winsize) size {
	want := size{rows: int(current.Rows)}
	rows := 0
	for _, lease := range s.leases {
		if want.columns == 0 || lease.Columns < want.columns {
			want.columns = lease.Columns
		}
		if lease.Rows > 0 && (rows == 0 || lease.Rows < rows) {
			rows = lease.Rows
		}
	}
	switch {
	case rows > 0:
		want.rows = rows
	case s.applied.rows != s.baseline.rows:
		want.rows = s.baseline.rows
	}
	return want
}

// reconcileNeededLocked estimates, from what was last applied, whether the
// lease set now asks for a different size.
func (s *paneState) reconcileNeededLocked() bool {
	estimate := Winsize{Columns: uint16(s.applied.columns), Rows: uint16(s.applied.rows)}
	return s.leaseTargetLocked(estimate) != s.applied
}

func (s *paneState) commitLocked(current Winsize, want size, now time.Time) {
	if _, changed := resized(current, want); changed {
		s.resizedAt = now
	}
	s.applied = want
}

func sizeOf(value Winsize) size {
	return size{columns: int(value.Columns), rows: int(value.Rows)}
}

// resized returns current with want's rows and columns. The pixel fields are
// preserved; a dimension that is not changing is written back with the value
// just read through the same descriptor.
func resized(current Winsize, want size) (Winsize, bool) {
	next := current
	next.Columns = uint16(want.columns)
	next.Rows = uint16(want.rows)
	return next, next != current
}

func cleanupBackoff(failedAttempts int) time.Duration {
	delay := cleanupFirstBackoff
	for range failedAttempts - 1 {
		delay *= 2
		if delay >= cleanupMaxBackoff {
			return cleanupMaxBackoff
		}
	}
	return delay
}
