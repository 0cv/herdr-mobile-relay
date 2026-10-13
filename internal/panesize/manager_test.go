package panesize

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type fakePane struct {
	terminalID string
	shellPID   int
	fgPID      int
	closed     bool
}

type herdrCall struct {
	method string
	paneID string
	at     time.Time
}

// fakeHerdr answers pane.get and pane.process_info from mutable pane records.
type fakeHerdr struct {
	mu    sync.Mutex
	clock *fakeClock
	panes map[string]*fakePane
	fail  map[string]error
	block map[string]bool
	// cost advances the fake clock per call, modelling slow lookups.
	cost  time.Duration
	calls []herdrCall
}

func (f *fakeHerdr) call(ctx context.Context, method, paneID string) (fakePane, error) {
	f.mu.Lock()
	f.calls = append(f.calls, herdrCall{method: method, paneID: paneID, at: f.clock.Now()})
	failure, block, cost := f.fail[paneID], f.block[paneID], f.cost
	pane := f.panes[paneID]
	var snapshot fakePane
	if pane != nil {
		snapshot = *pane
	}
	f.mu.Unlock()
	if cost > 0 {
		f.clock.Advance(cost)
	}
	if block {
		<-ctx.Done()
		return fakePane{}, ctx.Err()
	}
	if failure != nil {
		return fakePane{}, failure
	}
	if pane == nil || snapshot.closed {
		return fakePane{}, fmt.Errorf("Herdr socket API: %w", &herdr.CLIError{Code: "pane_not_found", Message: "pane not found"})
	}
	return snapshot, nil
}

func (f *fakeHerdr) PaneGet(ctx context.Context, paneID string) (*herdr.Pane, error) {
	pane, err := f.call(ctx, "pane.get", paneID)
	if err != nil {
		return nil, err
	}
	return &herdr.Pane{ID: paneID, TerminalID: pane.terminalID}, nil
}

func (f *fakeHerdr) PaneProcessInfoFresh(ctx context.Context, paneID string) (*herdr.PaneProcessInfo, error) {
	pane, err := f.call(ctx, "pane.process_info", paneID)
	if err != nil {
		return nil, err
	}
	info := &herdr.PaneProcessInfo{PaneID: paneID, ShellPID: pane.shellPID}
	if pane.fgPID > 0 {
		info.ForegroundProcessGroupID = pane.fgPID
		info.ForegroundProcesses = []herdr.PaneProcess{{PID: pane.fgPID, Name: "agent"}}
	}
	return info, nil
}

func (f *fakeHerdr) update(paneID string, change func(*fakePane)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.panes[paneID] == nil {
		f.panes[paneID] = &fakePane{}
	}
	change(f.panes[paneID])
}

func (f *fakeHerdr) setFailure(paneID string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		delete(f.fail, paneID)
		return
	}
	f.fail[paneID] = err
}

func (f *fakeHerdr) setBlock(paneID string, block bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.block[paneID] = block
}

func (f *fakeHerdr) callsSince(mark int) []herdrCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls[mark:])
}

func (f *fakeHerdr) mark() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type fakeTTYs struct {
	mu    sync.Mutex
	byPID map[int]string
}

func (f *fakeTTYs) TTYPath(_ context.Context, pid int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if path := f.byPID[pid]; path != "" {
		return path, nil
	}
	return "", ErrTTYUnavailable
}

func (f *fakeTTYs) set(pid int, path string) {
	f.mu.Lock()
	f.byPID[pid] = path
	f.mu.Unlock()
}

// fakeDevice is one PTY instance. A held fakeTerminal keeps pointing at the
// instance it opened even when another device later appears at the path,
// which is how a real descriptor behaves.
type fakeDevice struct {
	rdev   uint64
	char   bool
	size   Winsize
	writes []Winsize
	// before records the size each write replaced.
	before []Winsize
	setErr error
}

type fakeOps struct {
	mu      sync.Mutex
	devices map[string]*fakeDevice
	opens   int
	closes  int
}

func (o *fakeOps) Open(path string) (Terminal, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	device := o.devices[path]
	if device == nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: syscall.ENOENT}
	}
	o.opens++
	return &fakeTerminal{ops: o, device: device}, nil
}

func (o *fakeOps) put(path string, device *fakeDevice) {
	o.mu.Lock()
	o.devices[path] = device
	o.mu.Unlock()
}

type fakeTerminal struct {
	ops    *fakeOps
	device *fakeDevice
}

func (t *fakeTerminal) Device() (uint64, bool, error) {
	t.ops.mu.Lock()
	defer t.ops.mu.Unlock()
	return t.device.rdev, t.device.char, nil
}

func (t *fakeTerminal) Size() (Winsize, error) {
	t.ops.mu.Lock()
	defer t.ops.mu.Unlock()
	return t.device.size, nil
}

func (t *fakeTerminal) SetSize(size Winsize) error {
	t.ops.mu.Lock()
	defer t.ops.mu.Unlock()
	if t.device.setErr != nil {
		return t.device.setErr
	}
	t.device.before = append(t.device.before, t.device.size)
	t.device.writes = append(t.device.writes, size)
	t.device.size = size
	return nil
}

func (t *fakeTerminal) Close() error {
	t.ops.mu.Lock()
	t.ops.closes++
	t.ops.mu.Unlock()
	return nil
}

// beforeSetOps pauses at the actual write boundary without holding either
// bookkeeping or the fake device mutex, so a disconnect can run concurrently.
type beforeSetOps struct {
	TerminalOps
	beforeSet func()
}

func (o beforeSetOps) Open(path string) (Terminal, error) {
	terminal, err := o.TerminalOps.Open(path)
	if err != nil {
		return nil, err
	}
	return beforeSetTerminal{Terminal: terminal, beforeSet: o.beforeSet}, nil
}

type beforeSetTerminal struct {
	Terminal
	beforeSet func()
}

func (t beforeSetTerminal) SetSize(size Winsize) error {
	t.beforeSet()
	return t.Terminal.SetSize(size)
}

type logRecorder struct {
	mu      sync.Mutex
	records []slog.Record
}

func (r *logRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *logRecorder) Handle(_ context.Context, record slog.Record) error {
	r.mu.Lock()
	r.records = append(r.records, record.Clone())
	r.mu.Unlock()
	return nil
}

func (r *logRecorder) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *logRecorder) WithGroup(string) slog.Handler      { return r }

func (r *logRecorder) count(level slog.Level, fragment string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, record := range r.records {
		if record.Level == level && strings.Contains(record.Message, fragment) {
			count++
		}
	}
	return count
}

type fixture struct {
	t     *testing.T
	clock *fakeClock
	herdr *fakeHerdr
	ttys  *fakeTTYs
	ops   *fakeOps
	logs  *logRecorder
	m     *Manager
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	clock := &fakeClock{now: time.Unix(10_000, 0)}
	f := &fixture{
		t:     t,
		clock: clock,
		herdr: &fakeHerdr{clock: clock, panes: map[string]*fakePane{}, fail: map[string]error{}, block: map[string]bool{}},
		ttys:  &fakeTTYs{byPID: map[int]string{}},
		ops:   &fakeOps{devices: map[string]*fakeDevice{}},
		logs:  &logRecorder{},
	}
	f.m = newManager(f.herdr, f.ttys, f.ops, LeaseTTL, ReleaseGrace, clock.Now, slog.New(f.logs))
	f.m.opTimeout = time.Second
	return f
}

// addPane creates a pane whose shell runs on tty with a 120x40 window.
func (f *fixture) addPane(paneID string, shellPID int, tty string, rdev uint64) *fakeDevice {
	f.herdr.update(paneID, func(pane *fakePane) {
		pane.terminalID = "terminal-" + paneID
		pane.shellPID = shellPID
	})
	f.ttys.set(shellPID, tty)
	device := &fakeDevice{rdev: rdev, char: true, size: Winsize{Rows: 40, Columns: 120, XPixel: 960, YPixel: 640}}
	f.ops.put(tty, device)
	return device
}

func (f *fixture) acquire(clientID, paneID string, columns, rows int) (int, int) {
	f.t.Helper()
	appliedColumns, appliedRows, err := f.m.Acquire(context.Background(), clientID, paneID, columns, rows)
	if err != nil {
		f.t.Fatalf("Acquire(%s, %s, %d, %d) error = %v", clientID, paneID, columns, rows, err)
	}
	return appliedColumns, appliedRows
}

func (f *fixture) state(paneID string) *paneState {
	f.m.mu.Lock()
	defer f.m.mu.Unlock()
	return f.m.panes[paneID]
}

func (f *fixture) tracked() int {
	f.m.mu.Lock()
	defer f.m.mu.Unlock()
	return len(f.m.panes)
}

func (f *fixture) cleanup(paneID string) cleanupState {
	f.t.Helper()
	f.m.mu.Lock()
	defer f.m.mu.Unlock()
	state := f.m.panes[paneID]
	if state == nil || state.cleanup == nil {
		f.t.Fatalf("%s has no pending cleanup", paneID)
	}
	return *state.cleanup
}

// sweepFor advances the clock in sweep-sized steps.
func (f *fixture) sweepFor(total, step time.Duration) {
	for elapsed := time.Duration(0); elapsed < total; elapsed += step {
		f.clock.Advance(step)
		f.m.SweepExpired(context.Background())
	}
}

func (f *fixture) writes(device *fakeDevice) []Winsize {
	f.ops.mu.Lock()
	defer f.ops.mu.Unlock()
	return slices.Clone(device.writes)
}

func (f *fixture) size(device *fakeDevice) Winsize {
	f.ops.mu.Lock()
	defer f.ops.mu.Unlock()
	return device.size
}

func (f *fixture) setSize(device *fakeDevice, columns, rows uint16) {
	f.ops.mu.Lock()
	device.size.Columns, device.size.Rows = columns, rows
	f.ops.mu.Unlock()
}

func (f *fixture) assertDescriptorsClosed() {
	f.t.Helper()
	f.ops.mu.Lock()
	defer f.ops.mu.Unlock()
	if f.ops.opens != f.ops.closes {
		f.t.Fatalf("opened %d terminal descriptors, closed %d", f.ops.opens, f.ops.closes)
	}
}

func TestAcquireValidatesBeforeWritingAndPreservesPixels(t *testing.T) {
	f := newFixture(t)
	device := f.addPane("pane-1", 321, "/dev/pts/7", 0x8807)

	columns, rows := f.acquire("client-a", "pane-1", 84, 0)
	if columns != 84 || rows != 40 {
		t.Fatalf("Acquire() = %dx%d, want 84x40", columns, rows)
	}
	if got := f.writes(device); !slices.Equal(got, []Winsize{{Rows: 40, Columns: 84, XPixel: 960, YPixel: 640}}) {
		t.Fatalf("writes = %+v", got)
	}
	// Lookup to find the path, then a fresh re-validation after the open.
	methods := []string{}
	for _, call := range f.herdr.callsSince(0) {
		methods = append(methods, call.method)
	}
	if !slices.Equal(methods, []string{"pane.get", "pane.process_info", "pane.get", "pane.process_info"}) {
		t.Fatalf("Herdr calls = %v", methods)
	}
	state := f.state("pane-1")
	if state.identity != (terminalIdentity{terminalID: "terminal-pane-1", shellPID: 321, ttyPath: "/dev/pts/7", rdev: 0x8807}) {
		t.Fatalf("identity = %+v", state.identity)
	}
	if columns, rows, ok := f.m.ReadGeometry("client-a", "pane-1"); !ok || columns != 84 || rows != 40 {
		t.Fatalf("ReadGeometry(owner) = %d, %d, %v", columns, rows, ok)
	}
	if _, _, ok := f.m.ReadGeometry("client-b", "pane-1"); ok {
		t.Fatal("a client without a lease received the leased read geometry")
	}
	f.assertDescriptorsClosed()
}

func TestAcquireRejectsMissingIdentityWithoutWriting(t *testing.T) {
	for name, setup := range map[string]func(*fixture){
		"no terminal_id": func(f *fixture) {
			f.herdr.update("pane-1", func(pane *fakePane) { pane.terminalID = "" })
		},
		"no shell or foreground process": func(f *fixture) {
			f.herdr.update("pane-1", func(pane *fakePane) { pane.shellPID = 0 })
		},
		"shell without TTY": func(f *fixture) { f.ttys.set(321, "") },
		"pane closed": func(f *fixture) {
			f.herdr.update("pane-1", func(pane *fakePane) { pane.closed = true })
		},
		"Herdr without pane.get": func(f *fixture) {
			f.herdr.setFailure("pane-1", fmt.Errorf("Herdr socket API: %w", &herdr.CLIError{Code: "unknown_method"}))
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			device := f.addPane("pane-1", 321, "/dev/pts/7", 1)
			setup(f)
			_, _, err := f.m.Acquire(context.Background(), "client-a", "pane-1", 80, 0)
			if !errors.Is(err, ErrIdentityUnavailable) && !errors.Is(err, ErrTerminalGone) {
				t.Fatalf("Acquire() error = %v", err)
			}
			if len(f.writes(device)) != 0 || f.tracked() != 0 {
				t.Fatalf("writes=%v tracked=%d", f.writes(device), f.tracked())
			}
			f.assertDescriptorsClosed()
		})
	}
}

func TestAcquireFallsBackToForegroundLeaderOnlyWithoutShell(t *testing.T) {
	f := newFixture(t)
	device := f.addPane("pane-1", 321, "/dev/pts/7", 1)
	f.herdr.update("pane-1", func(pane *fakePane) {
		pane.shellPID = 0
		pane.fgPID = 654
	})
	f.ttys.set(654, "/dev/pts/7")
	f.acquire("client-a", "pane-1", 80, 0)
	if state := f.state("pane-1"); state.identity.shellPID != 0 || state.identity.ttyPath != "/dev/pts/7" {
		t.Fatalf("identity = %+v", state.identity)
	}
	// A shell appearing later means the identity basis changed: no write.
	f.herdr.update("pane-1", func(pane *fakePane) { pane.shellPID = 999 })
	if _, _, err := f.m.Acquire(context.Background(), "client-a", "pane-1", 60, 0); !errors.Is(err, ErrTerminalGone) {
		t.Fatalf("renewal error = %v", err)
	}
	if got := f.writes(device); len(got) != 1 {
		t.Fatalf("writes = %+v", got)
	}
}

func TestRenewalAtSameSizeDoesNotWriteAndSettleWindowTracksChanges(t *testing.T) {
	f := newFixture(t)
	device := f.addPane("pane-1", 321, "/dev/pts/7", 1)
	if f.m.ResizedWithin("pane-1", 3*time.Second) {
		t.Fatal("untracked pane reported as resized")
	}
	f.acquire("client-a", "pane-1", 46, 0)
	if !f.m.ResizedWithin("pane-1", 3*time.Second) {
		t.Fatal("a size change did not open the settle window")
	}
	f.clock.Advance(5 * time.Second)
	for range 12 {
		f.clock.Advance(10 * time.Second)
		f.acquire("client-a", "pane-1", 46, 0)
	}
	if got := f.writes(device); len(got) != 1 {
		t.Fatalf("same-size renewals wrote the terminal: %+v", got)
	}
	if f.m.ResizedWithin("pane-1", 3*time.Second) {
		t.Fatal("a same-size renewal re-opened the settle window")
	}
	f.acquire("client-a", "pane-1", 44, 0)
	if !f.m.ResizedWithin("pane-1", 3*time.Second) {
		t.Fatal("a width change after renewals did not open the settle window")
	}
}

func TestClosedPaneDropsStateWithoutWritingOrRetrying(t *testing.T) {
	f := newFixture(t)
	device := f.addPane("pane-1", 321, "/dev/pts/7", 1)
	f.acquire("client-a", "pane-1", 80, 0)
	f.herdr.update("pane-1", func(pane *fakePane) { pane.closed = true })

	if err := f.m.Release(context.Background(), "client-a", "pane-1", true); err != nil {
		t.Fatal(err)
	}
	if f.tracked() != 0 {
		t.Fatal("closed pane kept its lease state")
	}
	mark := f.herdr.mark()
	f.sweepFor(2*time.Minute, time.Second)
	if calls := f.herdr.callsSince(mark); len(calls) != 0 {
		t.Fatalf("closed pane was retried: %+v", calls)
	}
	if got := f.writes(device); len(got) != 1 {
		t.Fatalf("closed pane was written: %+v", got)
	}
	if got := f.logs.count(slog.LevelInfo, "dropped"); got != 1 {
		t.Fatalf("info logs = %d, want 1", got)
	}
	if got := f.logs.count(slog.LevelWarn, ""); got != 0 {
		t.Fatalf("warnings = %d, want 0", got)
	}
	f.assertDescriptorsClosed()
}

func TestTerminalReplacementIsConfirmedGone(t *testing.T) {
	f := newFixture(t)
	device := f.addPane("pane-1", 321, "/dev/pts/7", 1)
	f.acquire("client-a", "pane-1", 80, 0)
	f.herdr.update("pane-1", func(pane *fakePane) { pane.terminalID = "terminal-respawned" })
	if _, _, err := f.m.Acquire(context.Background(), "client-a", "pane-1", 70, 0); !errors.Is(err, ErrTerminalGone) {
		t.Fatalf("renewal error = %v", err)
	}
	if f.tracked() != 0 || len(f.writes(device)) != 1 {
		t.Fatalf("tracked=%d writes=%+v", f.tracked(), f.writes(device))
	}
}

// Every mutation path refuses a terminal whose identity no longer holds, even
// when another pane's PTY now answers at the cached path with the same
// device number. Neither the old nor the new terminal is written.
func TestPTYReuseIsNeverWritten(t *testing.T) {
	type operation struct {
		setup func(*fixture)
		run   func(*fixture) error
	}
	operations := map[string]operation{
		"renew": {
			setup: func(f *fixture) { f.acquire("client-a", "pane-1", 80, 0) },
			run: func(f *fixture) error {
				_, _, err := f.m.Acquire(context.Background(), "client-a", "pane-1", 80, 0)
				if !errors.Is(err, ErrTerminalGone) {
					return fmt.Errorf("renewal error = %v, want terminal gone", err)
				}
				return nil
			},
		},
		"reconcile": {
			setup: func(f *fixture) {
				f.acquire("client-a", "pane-1", 80, 0)
				f.acquire("client-b", "pane-1", 60, 0)
			},
			run: func(f *fixture) error { return f.m.ReleaseClient(context.Background(), "client-b") },
		},
		"restore": {
			setup: func(f *fixture) { f.acquire("client-a", "pane-1", 80, 0) },
			run: func(f *fixture) error {
				return f.m.Release(context.Background(), "client-a", "pane-1", true)
			},
		},
		"restore after grace": {
			setup: func(f *fixture) {
				f.acquire("client-a", "pane-1", 80, 0)
				if err := f.m.Release(context.Background(), "client-a", "pane-1", false); err != nil {
					f.t.Fatal(err)
				}
			},
			run: func(f *fixture) error {
				f.sweepFor(ReleaseGrace+time.Second, time.Second)
				return nil
			},
		},
		"shutdown": {
			setup: func(f *fixture) { f.acquire("client-a", "pane-1", 80, 0) },
			run:   func(f *fixture) error { return f.m.Shutdown(context.Background()) },
		},
	}
	// Each mismatch puts another pane's PTY at /dev/pts/7 (same device
	// number unless stated) and changes what Herdr or ps reports.
	mismatches := map[string]func(f *fixture){
		"pane closed": func(f *fixture) {
			f.herdr.update("pane-1", func(pane *fakePane) { pane.closed = true })
		},
		"terminal_id": func(f *fixture) {
			f.herdr.update("pane-1", func(pane *fakePane) { pane.terminalID = "terminal-other" })
		},
		"shell_pid": func(f *fixture) {
			f.herdr.update("pane-1", func(pane *fakePane) { pane.shellPID = 900 })
			f.ttys.set(900, "/dev/pts/9")
		},
		"tty": func(f *fixture) { f.ttys.set(321, "/dev/pts/9") },
		"rdev": func(f *fixture) {
			f.ops.put("/dev/pts/7", &fakeDevice{rdev: 2, char: true, size: Winsize{Rows: 24, Columns: 80}})
		},
		"device removed": func(f *fixture) { f.ops.put("/dev/pts/7", nil) },
	}
	for operationName, op := range operations {
		for mismatchName, mismatch := range mismatches {
			t.Run(operationName+"/"+mismatchName, func(t *testing.T) {
				f := newFixture(t)
				original := f.addPane("pane-1", 321, "/dev/pts/7", 1)
				op.setup(f)
				writesBefore := len(f.writes(original))
				reused := &fakeDevice{rdev: 1, char: true, size: Winsize{Rows: 24, Columns: 80}}
				f.ops.put("/dev/pts/7", reused)
				f.herdr.update("pane-2", func(pane *fakePane) {
					pane.terminalID = "terminal-pane-2"
					pane.shellPID = 777
				})
				f.ttys.set(777, "/dev/pts/7")
				mismatch(f)

				if err := op.run(f); err != nil {
					t.Fatal(err)
				}
				if got := f.writes(original); len(got) != writesBefore {
					t.Fatalf("original terminal written after the mismatch: %+v", got[writesBefore:])
				}
				if got := f.writes(reused); len(got) != 0 {
					t.Fatalf("reused PTY was written: %+v", got)
				}
				if f.tracked() != 0 {
					t.Fatal("lease state survived a confirmed identity mismatch")
				}
				if got := f.logs.count(slog.LevelInfo, "dropped"); got != 1 {
					t.Fatalf("info logs = %d, want 1", got)
				}
				mark := f.herdr.mark()
				f.sweepFor(time.Minute, time.Second)
				if calls := f.herdr.callsSince(mark); len(calls) != 0 {
					t.Fatalf("dropped pane was retried: %+v", calls)
				}
				f.assertDescriptorsClosed()
			})
		}
	}
}

func TestTransientFailureRetriesWithExactBackoffThenWarnsOnce(t *testing.T) {
	f := newFixture(t)
	device := f.addPane("pane-1", 321, "/dev/pts/7", 1)
	f.acquire("client-a", "pane-1", 80, 0)
	f.herdr.setFailure("pane-1", errors.New("Herdr socket API: read: i/o timeout"))
	start := f.clock.Now()
	mark := f.herdr.mark()

	if err := f.m.Release(context.Background(), "client-a", "pane-1", true); err != nil {
		t.Fatal(err)
	}
	f.sweepFor(time.Minute, 250*time.Millisecond)

	var attempts []time.Duration
	for _, call := range f.herdr.callsSince(mark) {
		if call.method == "pane.get" {
			attempts = append(attempts, call.at.Sub(start))
		}
	}
	want := []time.Duration{0, time.Second, 3 * time.Second, 7 * time.Second, 15 * time.Second}
	if !slices.Equal(attempts, want) {
		t.Fatalf("attempts at %v, want %v", attempts, want)
	}
	if got := f.writes(device); len(got) != 1 {
		t.Fatalf("an unverified terminal was written: %+v", got)
	}
	if f.tracked() != 0 {
		t.Fatal("exhausted cleanup kept its state")
	}
	if got := f.logs.count(slog.LevelWarn, "resize the pane locally"); got != 1 {
		t.Fatalf("exhaustion warnings = %d, want 1", got)
	}
	f.assertDescriptorsClosed()
}

func TestCleanupBudgetCountsLookupTime(t *testing.T) {
	f := newFixture(t)
	f.addPane("pane-1", 321, "/dev/pts/7", 1)
	f.acquire("client-a", "pane-1", 80, 0)
	f.herdr.setFailure("pane-1", errors.New("Herdr socket API: read: i/o timeout"))
	f.herdr.cost = 6 * time.Second
	start := f.clock.Now()
	mark := f.herdr.mark()

	if err := f.m.Release(context.Background(), "client-a", "pane-1", true); err != nil {
		t.Fatal(err)
	}
	f.sweepFor(time.Minute, 250*time.Millisecond)

	var attempts []time.Duration
	for _, call := range f.herdr.callsSince(mark) {
		attempts = append(attempts, call.at.Sub(start))
	}
	// Each attempt spends 6s in its lookup. The fifth attempt would start at
	// 31s+8s, past the 30s budget, so the cleanup ends after four.
	want := []time.Duration{0, 7 * time.Second, 15 * time.Second, 25 * time.Second}
	if !slices.Equal(attempts, want) {
		t.Fatalf("attempts at %v, want %v", attempts, want)
	}
	if f.tracked() != 0 || f.logs.count(slog.LevelWarn, "resize the pane locally") != 1 {
		t.Fatalf("tracked=%d warnings=%d", f.tracked(), f.logs.count(slog.LevelWarn, ""))
	}
}

func TestOnePaneFailingDoesNotBlockAnother(t *testing.T) {
	f := newFixture(t)
	f.m.opTimeout = 300 * time.Millisecond
	slow := f.addPane("pane-1", 321, "/dev/pts/7", 1)
	healthy := f.addPane("pane-2", 322, "/dev/pts/8", 2)
	f.acquire("client-a", "pane-1", 80, 0)
	f.acquire("client-a", "pane-2", 80, 0)
	f.herdr.setBlock("pane-1", true)
	f.herdr.setFailure("pane-2", errors.New("Herdr socket API: connection refused"))

	if err := f.m.ReleaseClient(context.Background(), "client-a"); err != nil {
		t.Fatal(err)
	}
	if f.cleanup("pane-1").attempts != 1 || f.cleanup("pane-2").attempts != 1 {
		t.Fatal("first attempts were not made on both panes")
	}
	f.herdr.setFailure("pane-2", nil)
	f.clock.Advance(time.Second)
	f.m.SweepExpired(context.Background())

	if got := f.size(healthy); got.Columns != 120 || got.Rows != 40 {
		t.Fatalf("healthy pane size = %+v, want 120x40", got)
	}
	if f.state("pane-2") != nil {
		t.Fatal("restored pane is still tracked")
	}
	if attempts := f.cleanup("pane-1").attempts; attempts != 2 {
		t.Fatalf("blocked pane attempts = %d, want 2", attempts)
	}
	if got := f.writes(slow); len(got) != 1 {
		t.Fatalf("blocked pane was written: %+v", got)
	}
	f.assertDescriptorsClosed()
}

func TestDisconnectBeforeLeaseInsertionCannotLeaveOrphan(t *testing.T) {
	for _, renewal := range []bool{false, true} {
		t.Run(fmt.Sprintf("renewal=%v", renewal), func(t *testing.T) {
			f := newFixture(t)
			device := f.addPane("pane-1", 321, "/dev/pts/7", 1)
			if renewal {
				f.acquire("client-a", "pane-1", 80, 0)
			}
			writes := len(f.writes(device))
			beforeInsertion, resume := make(chan struct{}), make(chan struct{})
			pause := make(chan struct{}, 1)
			pause <- struct{}{}
			var resumeOnce sync.Once
			defer resumeOnce.Do(func() { close(resume) })
			// Acquire reads the clock after its first context check but before
			// locking bookkeeping to insert the lease. Pause exactly there.
			f.m.now = func() time.Time {
				select {
				case <-pause:
					close(beforeInsertion)
					<-resume
				default:
				}
				return f.clock.Now()
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				_, _, err := f.m.Acquire(ctx, "client-a", "pane-1", 60, 0)
				result <- err
			}()
			select {
			case <-beforeInsertion:
			case <-time.After(3 * time.Second):
				t.Fatal("acquisition did not reach the insertion boundary")
			}
			cancel()
			// Scan after cancellation but before insertion. A cancelled cleanup
			// context avoids waiting for the I/O lock held by this acquisition.
			if err := f.m.ReleaseClient(ctx, "client-a"); err != nil {
				t.Fatal(err)
			}
			resumeOnce.Do(func() { close(resume) })
			select {
			case err := <-result:
				if !errors.Is(err, ErrLeaseOwnerGone) {
					t.Fatalf("Acquire() error = %v, want disconnected owner", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("acquisition did not finish")
			}
			if got := f.writes(device); len(got) != writes {
				t.Fatalf("disconnected acquisition wrote the terminal: %+v", got[writes:])
			}
			if _, _, owned := f.m.ReadGeometry("client-a", "pane-1"); owned {
				t.Fatal("disconnect left an orphan lease")
			}
			f.m.SweepExpired(context.Background())
			if f.tracked() != 0 || f.size(device).Columns != 120 {
				t.Fatal("disconnect did not finish restoring the pane")
			}
			f.assertDescriptorsClosed()
		})
	}
}

func TestBusyPaneCleanupDeadlineStartsWhenLastLeaseEnds(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		t.Run(fmt.Sprintf("disconnect=%v", disconnect), func(t *testing.T) {
			f := newFixture(t)
			f.addPane("pane-1", 321, "/dev/pts/7", 1)
			f.acquire("client-a", "pane-1", 80, 0)
			state := f.state("pane-1")
			if err := state.acquire(context.Background()); err != nil {
				t.Fatal(err)
			}
			var releaseOnce sync.Once
			defer releaseOnce.Do(state.release)
			ended := f.clock.Now()
			ctx, cancel := context.WithCancel(context.Background())
			cancel() // settle cannot obtain the busy pane's I/O lock
			var err error
			if disconnect {
				err = f.m.ReleaseClient(ctx, "client-a")
			} else {
				err = f.m.Release(ctx, "client-a", "pane-1", true)
			}
			if err != nil {
				t.Fatal(err)
			}
			pending := f.cleanup("pane-1")
			if pending.attempts != 0 || !pending.deadline.Equal(ended.Add(CleanupBudget)) {
				t.Fatalf("cleanup after lease end = %+v", pending)
			}
			f.clock.Advance(CleanupBudget)
			f.m.SweepExpired(context.Background()) // busy, but must not reset the budget
			if after := f.cleanup("pane-1"); !after.deadline.Equal(pending.deadline) {
				t.Fatalf("busy sweep extended cleanup: %+v", after)
			}
			releaseOnce.Do(state.release)
			f.m.SweepExpired(context.Background())
			if f.tracked() != 0 || f.logs.count(slog.LevelWarn, "not restored") != 1 {
				t.Fatal("cleanup outlived its deadline after the busy lock was released")
			}
		})
	}
}

func TestAcquireCompletionPreservesConcurrentReleaseCleanup(t *testing.T) {
	for _, event := range []struct{ renewal, disconnect bool }{
		{false, false}, {true, false}, {false, true}, {true, true},
	} {
		t.Run(fmt.Sprintf("renewal=%v/disconnect=%v", event.renewal, event.disconnect), func(t *testing.T) {
			f := newFixture(t)
			device := f.addPane("pane-1", 321, "/dev/pts/7", 1)
			if event.renewal {
				f.acquire("client-a", "pane-1", 80, 0)
			}
			beforeWrite, resume := make(chan struct{}), make(chan struct{})
			var resumeOnce sync.Once
			defer resumeOnce.Do(func() { close(resume) })
			f.m.ops = beforeSetOps{TerminalOps: f.ops, beforeSet: func() {
				close(beforeWrite)
				<-resume
			}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				_, _, err := f.m.Acquire(ctx, "client-a", "pane-1", 60, 0)
				result <- err
			}()
			select {
			case <-beforeWrite:
			case <-time.After(3 * time.Second):
				t.Fatal("acquisition did not reach the write boundary")
			}
			cleanupCtx, cancelCleanup := context.WithCancel(context.Background())
			cancelCleanup() // do not wait on the acquisition's I/O lock
			var err error
			if event.disconnect {
				cancel()
				err = f.m.ReleaseClient(cleanupCtx, "client-a")
			} else {
				err = f.m.Release(cleanupCtx, "client-a", "pane-1", true)
			}
			if err != nil {
				t.Fatal(err)
			}
			pending := f.cleanup("pane-1")
			f.clock.Advance(time.Second) // time passes while the write finishes
			resumeOnce.Do(func() { close(resume) })
			select {
			case err := <-result:
				if err != nil {
					t.Fatalf("in-flight write did not finish: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("acquisition did not finish")
			}
			after := f.cleanup("pane-1")
			if after.generation != pending.generation ||
				!after.deadline.Equal(pending.deadline) || after.attempts != 0 {
				t.Fatalf("commit replaced release cleanup: before=%+v after=%+v", pending, after)
			}
			if _, _, owned := f.m.ReadGeometry("client-a", "pane-1"); owned {
				t.Fatal("completion resurrected the released lease")
			}
			writes, lookups := len(f.writes(device)), f.herdr.mark()
			f.clock.Advance(CleanupBudget - time.Second)
			f.m.SweepExpired(context.Background())
			if f.tracked() != 0 || len(f.writes(device)) != writes ||
				f.herdr.mark() != lookups || f.logs.count(slog.LevelWarn, "not restored") != 1 {
				t.Fatal("expired release budget allowed another restore attempt")
			}
			f.assertDescriptorsClosed()
		})
	}
}

func TestDelayedSweepDoesNotRestartExpiryCleanupBudget(t *testing.T) {
	for _, grace := range []bool{false, true} {
		t.Run(fmt.Sprintf("grace=%v", grace), func(t *testing.T) {
			f := newFixture(t)
			device := f.addPane("pane-1", 321, "/dev/pts/7", 1)
			f.acquire("client-a", "pane-1", 80, 0)
			lapse := LeaseTTL
			if grace {
				_ = f.m.Release(context.Background(), "client-a", "pane-1", false)
				lapse = ReleaseGrace
			}
			f.clock.Advance(lapse + CleanupBudget)
			f.m.SweepExpired(context.Background())
			if f.tracked() != 0 || len(f.writes(device)) != 1 ||
				f.logs.count(slog.LevelWarn, "not restored") != 1 {
				t.Fatal("delayed sweep gave an expired lease a fresh cleanup budget")
			}
		})
	}
}

func TestReleaseBudgetIgnoresOtherAlreadyExpiredLeases(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		t.Run(fmt.Sprintf("disconnect=%v", disconnect), func(t *testing.T) {
			f := newFixture(t)
			device := f.addPane("pane-1", 321, "/dev/pts/7", 1)
			f.acquire("departed", "pane-1", 80, 0)
			f.clock.Advance(time.Minute)
			f.acquire("client-a", "pane-1", 60, 0)
			f.clock.Advance(time.Minute + 31*time.Second)
			// departed expired over thirty seconds ago, but client-a is live.
			// Ending client-a now must not start its budget at departed's expiry.
			if disconnect {
				_ = f.m.ReleaseClient(context.Background(), "client-a")
			} else {
				_ = f.m.Release(context.Background(), "client-a", "pane-1", true)
			}
			if f.tracked() != 0 || f.size(device).Columns != 120 ||
				f.logs.count(slog.LevelWarn, "not restored") != 0 {
				t.Fatal("another expired lease prevented an immediate restore")
			}
		})
	}
}

func TestRenewalAttemptsDoNotExtendCleanup(t *testing.T) {
	f := newFixture(t)
	f.addPane("pane-1", 321, "/dev/pts/7", 1)
	f.acquire("client-a", "pane-1", 80, 0)
	f.herdr.setFailure("pane-1", errors.New("Herdr socket API: read: i/o timeout"))
	if err := f.m.Release(context.Background(), "client-a", "pane-1", true); err != nil {
		t.Fatal(err)
	}
	before := f.cleanup("pane-1")
	generation := f.state("pane-1").generation

	f.clock.Advance(500 * time.Millisecond)
	for range 3 {
		if _, _, err := f.m.Acquire(context.Background(), "client-a", "pane-1", 80, 0); !errors.Is(err, ErrTerminalUnverified) {
			t.Fatalf("renewal against an unverifiable pane = %v", err)
		}
		_ = f.m.Release(context.Background(), "client-a", "pane-1", true)
		_ = f.m.Release(context.Background(), "client-b", "pane-1", false)
		_ = f.m.ReleaseClient(context.Background(), "client-a")
	}
	after := f.cleanup("pane-1")
	if after.attempts != before.attempts || !after.next.Equal(before.next) ||
		!after.deadline.Equal(before.deadline) || f.state("pane-1").generation != generation {
		t.Fatalf("cleanup changed from %+v to %+v", before, after)
	}
	if len(f.state("pane-1").leases) != 0 {
		t.Fatal("a failed renewal left a lease behind")
	}
}

func TestNewAcquisitionSupersedesPendingCleanup(t *testing.T) {
	f := newFixture(t)
	device := f.addPane("pane-1", 321, "/dev/pts/7", 1)
	f.acquire("client-a", "pane-1", 80, 0)
	f.herdr.setFailure("pane-1", errors.New("Herdr socket API: read: i/o timeout"))
	if err := f.m.Release(context.Background(), "client-a", "pane-1", true); err != nil {
		t.Fatal(err)
	}
	state := f.state("pane-1")
	staleGeneration := f.cleanup("pane-1").generation

	f.herdr.setFailure("pane-1", nil)
	f.clock.Advance(500 * time.Millisecond)
	f.acquire("client-b", "pane-1", 60, 0)
	f.m.mu.Lock()
	superseded := state.cleanup == nil && state.generation == staleGeneration+1 &&
		state.baseline == size{columns: 120, rows: 40} && len(state.leases) == 1
	f.m.mu.Unlock()
	if !superseded {
		t.Fatalf("cleanup was not superseded with the original baseline: %+v", state)
	}

	// A cleanup attempt planned before the new lease must not write over it
	// or delete it, however late it runs.
	writes := len(f.writes(device))
	if err := state.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.m.attemptCleanup(context.Background(), "pane-1", state, staleGeneration)
	state.release()
	if got := f.writes(device); len(got) != writes || f.state("pane-1") != state {
		t.Fatalf("stale cleanup acted: writes=%+v tracked=%v", got[writes:], f.state("pane-1") != nil)
	}
	f.sweepFor(time.Minute, time.Second)
	if got := f.size(device); got.Columns != 60 {
		t.Fatalf("leased pane size = %+v", got)
	}

	if err := f.m.Release(context.Background(), "client-b", "pane-1", true); err != nil {
		t.Fatal(err)
	}
	if got := f.size(device); got.Columns != 120 || got.Rows != 40 {
		t.Fatalf("restored size = %+v, want the original 120x40", got)
	}
}

func TestRestoreKeepsLocallyChangedDimensions(t *testing.T) {
	f := newFixture(t)
	device := f.addPane("pane-1", 321, "/dev/pts/7", 1)
	f.acquire("client-a", "pane-1", 80, 30)
	if got := f.size(device); got.Columns != 80 || got.Rows != 30 {
		t.Fatalf("leased size = %+v", got)
	}
	// The laptop widened the pane while the phone held it; rows untouched.
	f.setSize(device, 100, 30)
	if err := f.m.Release(context.Background(), "client-a", "pane-1", true); err != nil {
		t.Fatal(err)
	}
	if got := f.size(device); got.Columns != 100 || got.Rows != 40 {
		t.Fatalf("restored size = %+v, want local 100 columns and baseline 40 rows", got)
	}
}

func TestLocalChangeDuringLeaseBecomesBaseline(t *testing.T) {
	f := newFixture(t)
	device := f.addPane("pane-1", 321, "/dev/pts/7", 1)
	f.acquire("client-a", "pane-1", 80, 0)
	f.setSize(device, 150, 50)
	f.clock.Advance(10 * time.Second)
	f.acquire("client-a", "pane-1", 80, 0)
	if got := f.size(device); got.Columns != 80 || got.Rows != 50 {
		t.Fatalf("renewed size = %+v, want 80x50", got)
	}
	if err := f.m.Release(context.Background(), "client-a", "pane-1", true); err != nil {
		t.Fatal(err)
	}
	if got := f.size(device); got.Columns != 150 || got.Rows != 50 {
		t.Fatalf("restored size = %+v, want the local 150x50", got)
	}
}

func TestWidthOnlyLeaseNeverChangesRows(t *testing.T) {
	f := newFixture(t)
	device := f.addPane("pane-1", 321, "/dev/pts/7", 1)
	f.acquire("client-a", "pane-1", 80, 0)
	f.setSize(device, 80, 35)
	f.clock.Advance(10 * time.Second)
	f.acquire("client-a", "pane-1", 70, 0)
	f.acquire("client-b", "pane-1", 60, 0)
	if err := f.m.ReleaseClient(context.Background(), "client-b"); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Release(context.Background(), "client-a", "pane-1", true); err != nil {
		t.Fatal(err)
	}
	f.ops.mu.Lock()
	writes, before := slices.Clone(device.writes), slices.Clone(device.before)
	f.ops.mu.Unlock()
	if len(writes) < 4 {
		t.Fatalf("writes = %+v", writes)
	}
	for index, write := range writes {
		if write.Rows != before[index].Rows || write.XPixel != 960 || write.YPixel != 640 {
			t.Fatalf("write %d changed rows or pixels: %+v over %+v", index, write, before[index])
		}
	}
	if got := f.size(device); got.Columns != 120 || got.Rows != 35 {
		t.Fatalf("restored size = %+v, want 120x35", got)
	}
}

func TestLapsedRowLeaseRestoresHeightOnly(t *testing.T) {
	f := newFixture(t)
	device := f.addPane("pane-1", 321, "/dev/pts/7", 1)
	f.acquire("client-a", "pane-1", 80, 30)
	columns, rows := f.acquire("client-a", "pane-1", 80, 0)
	if columns != 80 || rows != 40 {
		t.Fatalf("width-only renewal = %dx%d, want 80x40", columns, rows)
	}
	if got := f.size(device); got.Columns != 80 || got.Rows != 40 {
		t.Fatalf("size = %+v", got)
	}
}

func TestMultipleClientsApplyMinimumAndRestoreWhenLastLeaves(t *testing.T) {
	f := newFixture(t)
	device := f.addPane("pane-1", 321, "/dev/pts/7", 1)
	f.acquire("client-a", "pane-1", 60, 0)
	if columns, _ := f.acquire("client-b", "pane-1", 80, 0); columns != 60 {
		t.Fatalf("aggregate = %d, want 60", columns)
	}
	if got := f.writes(device); len(got) != 1 {
		t.Fatalf("a wider second lease wrote the terminal: %+v", got)
	}
	if err := f.m.ReleaseClient(context.Background(), "client-a"); err != nil {
		t.Fatal(err)
	}
	if got := f.size(device); got.Columns != 80 {
		t.Fatalf("after the narrowest client left: %+v, want 80", got)
	}
	if err := f.m.ReleaseClient(context.Background(), "client-b"); err != nil {
		t.Fatal(err)
	}
	if got := f.size(device); got.Columns != 120 || f.tracked() != 0 {
		t.Fatalf("after the last client left: %+v tracked=%d", got, f.tracked())
	}
}

func TestReleaseGraceVersusImmediate(t *testing.T) {
	f := newFixture(t)
	device := f.addPane("pane-1", 321, "/dev/pts/7", 1)
	f.acquire("client-a", "pane-1", 80, 0)
	if err := f.m.Release(context.Background(), "client-a", "pane-1", false); err != nil {
		t.Fatal(err)
	}
	f.sweepFor(ReleaseGrace-time.Second, time.Second)
	if got := f.size(device); got.Columns != 80 {
		t.Fatalf("restored during the grace: %+v", got)
	}
	// Stepping back in during the grace keeps the size without a write.
	f.acquire("client-a", "pane-1", 80, 0)
	if got := f.writes(device); len(got) != 1 {
		t.Fatalf("re-entry during the grace wrote: %+v", got)
	}
	if err := f.m.Release(context.Background(), "client-a", "pane-1", false); err != nil {
		t.Fatal(err)
	}
	f.sweepFor(ReleaseGrace+time.Second, time.Second)
	if got := f.size(device); got.Columns != 120 {
		t.Fatalf("grace expiry did not restore: %+v", got)
	}

	f.acquire("client-a", "pane-1", 80, 0)
	if err := f.m.Release(context.Background(), "client-a", "pane-1", true); err != nil {
		t.Fatal(err)
	}
	if got := f.size(device); got.Columns != 120 || f.tracked() != 0 {
		t.Fatalf("immediate release did not restore at once: %+v", got)
	}
}

func TestLeaseExpiryRestoresWithoutRelease(t *testing.T) {
	f := newFixture(t)
	device := f.addPane("pane-1", 321, "/dev/pts/7", 1)
	f.acquire("client-a", "pane-1", 80, 0)
	f.sweepFor(LeaseTTL-time.Second, time.Second)
	if got := f.size(device); got.Columns != 80 {
		t.Fatalf("restored before the TTL: %+v", got)
	}
	f.sweepFor(2*time.Second, time.Second)
	if got := f.size(device); got.Columns != 120 || f.tracked() != 0 {
		t.Fatalf("TTL expiry did not restore: %+v", got)
	}
}

func TestReleaseClientRestoresEveryOwnedPane(t *testing.T) {
	f := newFixture(t)
	first := f.addPane("pane-1", 321, "/dev/pts/7", 1)
	second := f.addPane("pane-2", 322, "/dev/pts/8", 2)
	other := f.addPane("pane-3", 323, "/dev/pts/9", 3)
	f.acquire("client-a", "pane-1", 80, 0)
	f.acquire("client-a", "pane-2", 70, 20)
	f.acquire("client-b", "pane-3", 60, 0)
	if err := f.m.ReleaseClient(context.Background(), "client-a"); err != nil {
		t.Fatal(err)
	}
	for _, device := range []*fakeDevice{first, second} {
		if got := f.size(device); got.Columns != 120 || got.Rows != 40 {
			t.Fatalf("owned pane = %+v", got)
		}
	}
	if got := f.size(other); got.Columns != 60 || f.state("pane-3") == nil {
		t.Fatalf("another client's pane = %+v", got)
	}
}

func TestShutdownRestoresWithinDeadlineAndReportsUnresolved(t *testing.T) {
	f := newFixture(t)
	f.m.opTimeout = 5 * time.Second
	stuck := f.addPane("pane-1", 321, "/dev/pts/7", 1)
	healthy := f.addPane("pane-2", 322, "/dev/pts/8", 2)
	f.acquire("client-a", "pane-1", 80, 0)
	f.acquire("client-a", "pane-2", 80, 0)
	f.herdr.setBlock("pane-1", true)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := f.m.Shutdown(ctx)
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("shutdown ignored its deadline: %v", elapsed)
	}
	if err == nil || !strings.Contains(err.Error(), "pane-1") || strings.Contains(err.Error(), "pane-2") {
		t.Fatalf("Shutdown() error = %v, want only pane-1 unresolved", err)
	}
	if got := f.size(healthy); got.Columns != 120 {
		t.Fatalf("healthy pane = %+v", got)
	}
	if got := f.writes(stuck); len(got) != 1 {
		t.Fatalf("unverified pane was written: %+v", got)
	}
	if f.logs.count(slog.LevelWarn, "not restored at shutdown") != 1 {
		t.Fatal("unresolved panes were not logged")
	}
	if _, _, err := f.m.Acquire(context.Background(), "client-a", "pane-2", 80, 0); !errors.Is(err, ErrClosed) {
		t.Fatalf("Acquire after shutdown = %v", err)
	}
	f.assertDescriptorsClosed()
}

func TestWriteFailureLeavesNoLease(t *testing.T) {
	f := newFixture(t)
	device := f.addPane("pane-1", 321, "/dev/pts/7", 1)
	device.setErr = syscall.EIO
	if _, _, err := f.m.Acquire(context.Background(), "client-a", "pane-1", 80, 0); !errors.Is(err, ErrResizeFailed) {
		t.Fatalf("Acquire() error = %v", err)
	}
	if f.tracked() != 0 {
		t.Fatal("failed first acquisition left state behind")
	}
}

func TestUnavailableManagerTouchesNothing(t *testing.T) {
	f := newFixture(t)
	f.m.available = false
	device := f.addPane("pane-1", 321, "/dev/pts/7", 1)
	if _, _, err := f.m.Acquire(context.Background(), "client-a", "pane-1", 80, 0); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Acquire() error = %v", err)
	}
	if f.herdr.mark() != 0 || len(f.writes(device)) != 0 || f.tracked() != 0 {
		t.Fatal("an unavailable manager performed lookups or writes")
	}
	if NewManager(nil, nil).Available() {
		t.Fatal("a manager without a Herdr provider reports itself available")
	}
}

func TestInvalidLeaseArguments(t *testing.T) {
	f := newFixture(t)
	for _, test := range []struct {
		columns, rows int
		want          error
	}{
		{39, 0, ErrInvalidColumns}, {241, 0, ErrInvalidColumns},
		{80, 9, ErrInvalidRows}, {80, 121, ErrInvalidRows},
	} {
		if _, _, err := f.m.Acquire(context.Background(), "client-a", "pane-1", test.columns, test.rows); !errors.Is(err, test.want) {
			t.Fatalf("Acquire(%d, %d) = %v, want %v", test.columns, test.rows, err, test.want)
		}
	}
	if f.herdr.mark() != 0 {
		t.Fatal("invalid arguments reached Herdr")
	}
}

func TestPublicMessageHidesLookupDetail(t *testing.T) {
	detailed := unverified("pane.get", errors.New("dial unix /tmp/herdr.sock: connect: refused"))
	if got := PublicMessage(detailed); got != ErrTerminalUnverified.Error() {
		t.Fatalf("PublicMessage() = %q", got)
	}
	if got := PublicMessage(gone("terminal device number changed")); got != ErrTerminalGone.Error() {
		t.Fatalf("PublicMessage() = %q", got)
	}
}

func TestTTYPathRejectsUnsafeValues(t *testing.T) {
	for output, want := range map[string]string{
		"pts/7\n": "/dev/pts/7", "ttys003": "/dev/ttys003", "/dev/pts/2": "/dev/pts/2",
	} {
		if got, err := ttyPath([]byte(output)); err != nil || got != want {
			t.Fatalf("ttyPath(%q) = %q, %v", output, got, err)
		}
	}
	for _, output := range []string{"", "?", "??", "-", "../etc/passwd", "a b"} {
		if _, err := ttyPath([]byte(output)); !errors.Is(err, ErrTTYUnavailable) {
			t.Fatalf("ttyPath(%q) accepted", output)
		}
	}
}

func TestCleanupBackoffSchedule(t *testing.T) {
	var got []time.Duration
	for attempt := 1; attempt <= 6; attempt++ {
		got = append(got, cleanupBackoff(attempt))
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 10 * time.Second, 10 * time.Second}
	if !slices.Equal(got, want) {
		t.Fatalf("backoff = %v, want %v", got, want)
	}
}
