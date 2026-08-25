package panesize

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

type fakeProcessInfoProvider struct {
	infos map[string]*herdr.PaneProcessInfo
}

func (f *fakeProcessInfoProvider) PaneProcessInfo(_ context.Context, paneID string) (*herdr.PaneProcessInfo, error) {
	info := f.infos[paneID]
	if info == nil {
		return nil, errors.New("not found")
	}
	return info, nil
}

type runnerCall struct {
	name string
	args []string
}

type fakeCommandRunner struct {
	ttyByPID     map[int]string
	sizes        map[string]terminalSize
	calls        []runnerCall
	setValues    []int
	setRowValues []int
}

func (f *fakeCommandRunner) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, runnerCall{name: name, args: slices.Clone(args)})
	switch name {
	case "ps":
		if len(args) != 4 || args[0] != "-o" || args[1] != "tty=" || args[2] != "-p" {
			return nil, fmt.Errorf("unexpected ps arguments: %v", args)
		}
		pid, err := strconv.Atoi(args[3])
		if err != nil || f.ttyByPID[pid] == "" {
			return nil, errors.New("pid has no tty")
		}
		return []byte(f.ttyByPID[pid] + "\n"), nil
	case "stty":
		if len(args) < 3 || (args[0] != "-F" && args[0] != "-f") {
			return nil, fmt.Errorf("unexpected stty arguments: %v", args)
		}
		tty := args[1]
		size, ok := f.sizes[tty]
		if !ok {
			return nil, errors.New("unknown tty")
		}
		if args[2] == "size" {
			if len(args) != 3 {
				return nil, fmt.Errorf("unexpected stty size arguments: %v", args)
			}
			return []byte(fmt.Sprintf("%d %d\n", size.rows, size.columns)), nil
		}
		for i := 2; i < len(args); i += 2 {
			if i+1 >= len(args) {
				return nil, fmt.Errorf("unexpected stty arguments: %v", args)
			}
			value, err := strconv.Atoi(args[i+1])
			if err != nil {
				return nil, err
			}
			switch args[i] {
			case "cols":
				size.columns = value
				f.setValues = append(f.setValues, value)
			case "rows":
				size.rows = value
				f.setRowValues = append(f.setRowValues, value)
			default:
				return nil, fmt.Errorf("unexpected stty operation: %v", args)
			}
		}
		f.sizes[tty] = size
		return nil, nil
	default:
		return nil, fmt.Errorf("unexpected command: %s", name)
	}
}

func processInfo(paneID string, pid int) *herdr.PaneProcessInfo {
	return &herdr.PaneProcessInfo{
		PaneID:                   paneID,
		ForegroundProcessGroupID: pid,
		ForegroundProcesses:      []herdr.PaneProcess{{PID: pid, Name: "agent"}},
	}
}

func testManager(
	provider *fakeProcessInfoProvider,
	runner *fakeCommandRunner,
	now func() time.Time,
) *Manager {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return newManager(provider, runner, "linux", LeaseTTL, ReleaseGrace, now, logger)
}

func TestAcquireUsesForegroundTTYAndChangesColumnsOnly(t *testing.T) {
	now := time.Unix(100, 0)
	provider := &fakeProcessInfoProvider{infos: map[string]*herdr.PaneProcessInfo{
		"pane-1": processInfo("pane-1", 321),
	}}
	runner := &fakeCommandRunner{
		ttyByPID: map[int]string{321: "pts/7"},
		sizes:    map[string]terminalSize{"/dev/pts/7": {rows: 37, columns: 132}},
	}
	manager := testManager(provider, runner, func() time.Time { return now })

	applied, appliedRows, err := manager.Acquire(context.Background(), "transport-client-1", "pane-1", 84, 0)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if applied != 84 || appliedRows != 37 {
		t.Fatalf("Acquire() = %dx%d, want 84x37", applied, appliedRows)
	}
	if columns, ok := manager.ActiveColumns("pane-1"); !ok || columns != 84 {
		t.Fatalf("ActiveColumns() = %d, %v, want 84, true", columns, ok)
	}
	if rows, ok := manager.ActiveRows("pane-1"); !ok || rows != 37 {
		t.Fatalf("ActiveRows() = %d, %v, want 37, true", rows, ok)
	}
	if got := runner.sizes["/dev/pts/7"]; got.rows != 37 || got.columns != 84 {
		t.Fatalf("terminal size = %+v, want rows unchanged and 84 columns", got)
	}
	if len(runner.calls) != 3 || runner.calls[0].name != "ps" ||
		!slices.Equal(runner.calls[2].args, []string{"-F", "/dev/pts/7", "cols", "84"}) {
		t.Fatalf("command calls = %#v", runner.calls)
	}
}

// The settle window opens only when a lease actually changes the width:
// renewals at the same columns do not signal the application and must not
// re-open it.
func TestResizedWithinTracksActualColumnChanges(t *testing.T) {
	now := time.Unix(400, 0)
	provider := &fakeProcessInfoProvider{infos: map[string]*herdr.PaneProcessInfo{
		"pane-1": processInfo("pane-1", 621),
	}}
	runner := &fakeCommandRunner{
		ttyByPID: map[int]string{621: "pts/5"},
		sizes:    map[string]terminalSize{"/dev/pts/5": {rows: 48, columns: 151}},
	}
	manager := testManager(provider, runner, func() time.Time { return now })

	if manager.ResizedWithin("pane-1", 3*time.Second) {
		t.Fatal("untracked pane reported as resized")
	}
	if _, _, err := manager.Acquire(context.Background(), "client-a", "pane-1", 46, 0); err != nil {
		t.Fatal(err)
	}
	if !manager.ResizedWithin("pane-1", 3*time.Second) {
		t.Fatal("width change did not open the settle window")
	}

	now = now.Add(5 * time.Second)
	if manager.ResizedWithin("pane-1", 3*time.Second) {
		t.Fatal("settle window did not close after the timeout")
	}
	// Renewal at the same dimensions must only extend the lease. Running
	// `stty cols` again reaches the TTY resize syscall every ten seconds and
	// can make an agent repaint even though its geometry did not change.
	resizeWrites := len(runner.setValues)
	for renewal := range 12 {
		now = now.Add(10 * time.Second)
		if _, _, err := manager.Acquire(context.Background(), "client-a", "pane-1", 46, 0); err != nil {
			t.Fatalf("renewal %d: %v", renewal+1, err)
		}
	}
	if len(runner.setValues) != resizeWrites {
		t.Fatalf("same-width renewal issued another terminal resize: values=%v", runner.setValues)
	}
	if manager.ResizedWithin("pane-1", 3*time.Second) {
		t.Fatal("same-width renewal re-opened the settle window")
	}

	// A genuine width change re-opens it.
	if _, _, err := manager.Acquire(context.Background(), "client-a", "pane-1", 44, 0); err != nil {
		t.Fatal(err)
	}
	if !manager.ResizedWithin("pane-1", 3*time.Second) {
		t.Fatal("width change after renewal did not open the settle window")
	}
}

func TestMultipleClientsApplyMinimumAndRestoreBaselineOnRelease(t *testing.T) {
	now := time.Unix(200, 0)
	provider := &fakeProcessInfoProvider{infos: map[string]*herdr.PaneProcessInfo{
		"pane-1": processInfo("pane-1", 421),
	}}
	runner := &fakeCommandRunner{
		ttyByPID: map[int]string{421: "pts/8"},
		sizes:    map[string]terminalSize{"/dev/pts/8": {rows: 42, columns: 160}},
	}
	manager := testManager(provider, runner, func() time.Time { return now })

	if columns, _, err := manager.Acquire(context.Background(), "client-a", "pane-1", 110, 0); err != nil || columns != 110 {
		t.Fatalf("first Acquire() = %d, %v", columns, err)
	}
	if columns, _, err := manager.Acquire(context.Background(), "client-b", "pane-1", 76, 0); err != nil || columns != 76 {
		t.Fatalf("second Acquire() = %d, %v", columns, err)
	}
	if err := manager.Release(context.Background(), "client-b", "pane-1"); err != nil {
		t.Fatalf("Release(client-b) error = %v", err)
	}
	now = now.Add(ReleaseGrace)
	if err := manager.SweepExpired(context.Background()); err != nil {
		t.Fatalf("SweepExpired() after client-b error = %v", err)
	}
	if err := manager.Release(context.Background(), "client-a", "pane-1"); err != nil {
		t.Fatalf("Release(client-a) error = %v", err)
	}
	now = now.Add(ReleaseGrace)
	if err := manager.SweepExpired(context.Background()); err != nil {
		t.Fatalf("SweepExpired() after client-a error = %v", err)
	}
	if !slices.Equal(runner.setValues, []int{110, 76, 110, 160}) {
		t.Fatalf("applied columns = %v, want [110 76 110 160]", runner.setValues)
	}
	if len(manager.panes) != 0 {
		t.Fatalf("tracked panes = %d after final release", len(manager.panes))
	}
}

// Leaving a terminal on the phone and stepping back into it must not resize
// the pane: every SIGWINCH makes the agent re-render, and the phone withholds
// frames until that settles, which is what a stalled stream looks like.
func TestReleaseKeepsTheWidthForAReturningLeaseOwner(t *testing.T) {
	now := time.Unix(600, 0)
	provider := &fakeProcessInfoProvider{infos: map[string]*herdr.PaneProcessInfo{
		"pane-1": processInfo("pane-1", 821),
	}}
	runner := &fakeCommandRunner{
		ttyByPID: map[int]string{821: "pts/13"},
		sizes:    map[string]terminalSize{"/dev/pts/13": {rows: 44, columns: 180}},
	}
	manager := testManager(provider, runner, func() time.Time { return now })

	if _, _, err := manager.Acquire(context.Background(), "client-a", "pane-1", 84, 0); err != nil {
		t.Fatal(err)
	}
	if err := manager.Release(context.Background(), "client-a", "pane-1"); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	now = now.Add(ReleaseGrace - time.Second)
	if err := manager.SweepExpired(context.Background()); err != nil {
		t.Fatalf("SweepExpired() inside the grace error = %v", err)
	}
	if columns, ok := manager.ActiveColumns("pane-1"); !ok || columns != 84 {
		t.Fatalf("ActiveColumns() inside the grace = %d, %v, want 84, true", columns, ok)
	}
	if _, _, err := manager.Acquire(context.Background(), "client-a", "pane-1", 84, 0); err != nil {
		t.Fatalf("returning Acquire() error = %v", err)
	}
	if !slices.Equal(runner.setValues, []int{84}) {
		t.Fatalf("applied columns = %v, want one resize before the grace-window reacquisition", runner.setValues)
	}
	if manager.ResizedWithin("pane-1", time.Second) {
		t.Fatal("returning to the terminal reopened the resize settle window")
	}
}

func TestReleaseRestoresBaselineOnceTheGraceElapses(t *testing.T) {
	now := time.Unix(700, 0)
	provider := &fakeProcessInfoProvider{infos: map[string]*herdr.PaneProcessInfo{
		"pane-1": processInfo("pane-1", 921),
	}}
	runner := &fakeCommandRunner{
		ttyByPID: map[int]string{921: "pts/14"},
		sizes:    map[string]terminalSize{"/dev/pts/14": {rows: 48, columns: 190}},
	}
	manager := testManager(provider, runner, func() time.Time { return now })

	if _, _, err := manager.Acquire(context.Background(), "client-a", "pane-1", 90, 0); err != nil {
		t.Fatal(err)
	}
	if err := manager.Release(context.Background(), "client-a", "pane-1"); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	now = now.Add(ReleaseGrace)
	if err := manager.SweepExpired(context.Background()); err != nil {
		t.Fatalf("SweepExpired() error = %v", err)
	}
	if got := runner.sizes["/dev/pts/14"]; got.columns != 190 {
		t.Fatalf("restored size = %+v, want the 190 column baseline", got)
	}
	if len(manager.panes) != 0 {
		t.Fatalf("tracked panes = %d after the grace elapsed", len(manager.panes))
	}
}

func TestRefreshPreservesLocalColumnResizeAsNewBaseline(t *testing.T) {
	now := time.Unix(300, 0)
	provider := &fakeProcessInfoProvider{infos: map[string]*herdr.PaneProcessInfo{
		"pane-1": processInfo("pane-1", 521),
	}}
	runner := &fakeCommandRunner{
		ttyByPID: map[int]string{521: "pts/9"},
		sizes:    map[string]terminalSize{"/dev/pts/9": {rows: 45, columns: 120}},
	}
	manager := testManager(provider, runner, func() time.Time { return now })

	if _, _, err := manager.Acquire(context.Background(), "client-a", "pane-1", 80, 0); err != nil {
		t.Fatal(err)
	}
	runner.sizes["/dev/pts/9"] = terminalSize{rows: 51, columns: 150}
	now = now.Add(10 * time.Second)
	if _, _, err := manager.Acquire(context.Background(), "client-a", "pane-1", 80, 0); err != nil {
		t.Fatalf("refresh Acquire() error = %v", err)
	}
	if err := manager.Release(context.Background(), "client-a", "pane-1"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(ReleaseGrace)
	if err := manager.SweepExpired(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := runner.sizes["/dev/pts/9"]; got.rows != 51 || got.columns != 150 {
		t.Fatalf("restored size = %+v, want local 51 rows and 150 columns", got)
	}
}

func TestExpirySweepRestoresBaseline(t *testing.T) {
	now := time.Unix(400, 0)
	provider := &fakeProcessInfoProvider{infos: map[string]*herdr.PaneProcessInfo{
		"pane-1": processInfo("pane-1", 621),
	}}
	runner := &fakeCommandRunner{
		ttyByPID: map[int]string{621: "pts/10"},
		sizes:    map[string]terminalSize{"/dev/pts/10": {rows: 34, columns: 144}},
	}
	manager := testManager(provider, runner, func() time.Time { return now })

	if _, _, err := manager.Acquire(context.Background(), "client-a", "pane-1", 72, 0); err != nil {
		t.Fatal(err)
	}
	if columns, ok := manager.ActiveColumns("pane-1"); !ok || columns != 72 {
		t.Fatalf("ActiveColumns() before expiry = %d, %v, want 72, true", columns, ok)
	}
	now = now.Add(LeaseTTL)
	if columns, ok := manager.ActiveColumns("pane-1"); ok || columns != 0 {
		t.Fatalf("ActiveColumns() after expiry = %d, %v, want 0, false", columns, ok)
	}
	if err := manager.SweepExpired(context.Background()); err != nil {
		t.Fatalf("SweepExpired() error = %v", err)
	}
	if !slices.Equal(runner.setValues, []int{72, 144}) {
		t.Fatalf("applied columns = %v, want lease then baseline", runner.setValues)
	}
	if len(manager.panes) != 0 {
		t.Fatalf("tracked panes = %d after expiry", len(manager.panes))
	}
}

func TestReleaseClientAndShutdownCannotLeaveLeases(t *testing.T) {
	now := time.Unix(500, 0)
	provider := &fakeProcessInfoProvider{infos: map[string]*herdr.PaneProcessInfo{
		"pane-1": processInfo("pane-1", 721),
		"pane-2": processInfo("pane-2", 722),
	}}
	runner := &fakeCommandRunner{
		ttyByPID: map[int]string{721: "pts/11", 722: "pts/12"},
		sizes: map[string]terminalSize{
			"/dev/pts/11": {rows: 40, columns: 130},
			"/dev/pts/12": {rows: 50, columns: 170},
		},
	}
	manager := testManager(provider, runner, func() time.Time { return now })

	if _, _, err := manager.Acquire(context.Background(), "client-a", "pane-1", 75, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.Acquire(context.Background(), "client-a", "pane-2", 85, 0); err != nil {
		t.Fatal(err)
	}
	if err := manager.ReleaseClient(context.Background(), "client-a"); err != nil {
		t.Fatalf("ReleaseClient() error = %v", err)
	}
	if len(manager.panes) != 0 {
		t.Fatalf("tracked panes = %d after client disconnect", len(manager.panes))
	}

	if _, _, err := manager.Acquire(context.Background(), "client-b", "pane-1", 90, 0); err != nil {
		t.Fatal(err)
	}
	if err := manager.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if len(manager.panes) != 0 {
		t.Fatalf("tracked panes = %d after shutdown", len(manager.panes))
	}
	if _, _, err := manager.Acquire(context.Background(), "client-c", "pane-1", 90, 0); !errors.Is(err, ErrClosed) {
		t.Fatalf("Acquire() after shutdown error = %v, want ErrClosed", err)
	}
}

func TestAcquireRejectsInvalidColumnsAndNonTTYMetadata(t *testing.T) {
	now := time.Unix(600, 0)
	provider := &fakeProcessInfoProvider{infos: map[string]*herdr.PaneProcessInfo{
		"pane-1": processInfo("pane-1", 821),
	}}
	runner := &fakeCommandRunner{
		ttyByPID: map[int]string{821: "?"},
		sizes:    map[string]terminalSize{},
	}
	manager := testManager(provider, runner, func() time.Time { return now })

	if _, _, err := manager.Acquire(context.Background(), "client-a", "pane-1", MinColumns-1, 0); !errors.Is(err, ErrInvalidColumns) {
		t.Fatalf("invalid columns error = %v", err)
	}
	if _, _, err := manager.Acquire(context.Background(), "client-a", "pane-1", 80, MinRows-1); !errors.Is(err, ErrInvalidRows) {
		t.Fatalf("invalid rows error = %v", err)
	}
	if _, _, err := manager.Acquire(context.Background(), "client-a", "pane-1", 80, 0); !errors.Is(err, ErrTTYUnavailable) {
		t.Fatalf("non-TTY error = %v", err)
	}
	provider.infos["pane-1"] = &herdr.PaneProcessInfo{PaneID: "pane-1"}
	if _, _, err := manager.Acquire(context.Background(), "client-a", "pane-1", 80, 0); !errors.Is(err, ErrProcessUnavailable) {
		t.Fatalf("missing process metadata error = %v", err)
	}
}

func TestAcquireRejectsDisconnectedOwnerBeforeCreatingLease(t *testing.T) {
	now := time.Unix(700, 0)
	provider := &fakeProcessInfoProvider{infos: map[string]*herdr.PaneProcessInfo{
		"pane-1": processInfo("pane-1", 921),
	}}
	runner := &fakeCommandRunner{
		ttyByPID: map[int]string{921: "pts/13"},
		sizes:    map[string]terminalSize{"/dev/pts/13": {rows: 40, columns: 120}},
	}
	manager := testManager(provider, runner, func() time.Time { return now })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, _, err := manager.Acquire(ctx, "client-a", "pane-1", 80, 0); !errors.Is(err, ErrLeaseOwnerGone) {
		t.Fatalf("disconnected owner error = %v", err)
	}
	if len(runner.calls) != 0 || len(manager.panes) != 0 {
		t.Fatalf("disconnected owner created state: calls=%v panes=%d", runner.calls, len(manager.panes))
	}
}

// Rows ride the same lease: the smallest row constraint wins, width-only
// clients never lower it, and lapsed row leases give the pane its own
// height back while surviving width leases stay applied.
func TestRowLeasesApplyMinimumAndRestoreBaselineHeight(t *testing.T) {
	now := time.Unix(800, 0)
	provider := &fakeProcessInfoProvider{infos: map[string]*herdr.PaneProcessInfo{
		"pane-1": processInfo("pane-1", 951),
	}}
	runner := &fakeCommandRunner{
		ttyByPID: map[int]string{951: "pts/15"},
		sizes:    map[string]terminalSize{"/dev/pts/15": {rows: 64, columns: 180}},
	}
	manager := testManager(provider, runner, func() time.Time { return now })

	columns, rows, err := manager.Acquire(context.Background(), "phone-a", "pane-1", 84, 30)
	if err != nil || columns != 84 || rows != 30 {
		t.Fatalf("Acquire() = %dx%d, %v, want 84x30", columns, rows, err)
	}
	if got := runner.sizes["/dev/pts/15"]; got.rows != 30 || got.columns != 84 {
		t.Fatalf("terminal size = %+v, want 84x30", got)
	}
	if !manager.ResizedWithin("pane-1", time.Second) {
		t.Fatal("row lease did not open the settle window")
	}

	// A second, taller phone must not raise the applied height.
	if _, rows, err := manager.Acquire(context.Background(), "phone-b", "pane-1", 100, 44); err != nil || rows != 30 {
		t.Fatalf("second Acquire() rows = %d, %v, want 30", rows, err)
	}
	if got, ok := manager.ActiveRows("pane-1"); !ok || got != 30 {
		t.Fatalf("ActiveRows() = %d, %v, want 30, true", got, ok)
	}

	// A width-only client never constrains the height.
	if _, rows, err := manager.Acquire(context.Background(), "desk-c", "pane-1", 90, 0); err != nil || rows != 30 {
		t.Fatalf("width-only Acquire() rows = %d, %v, want 30", rows, err)
	}

	// A rows-only change re-opens the settle window like a width change.
	now = now.Add(5 * time.Second)
	if manager.ResizedWithin("pane-1", time.Second) {
		t.Fatal("settle window did not close")
	}
	if _, rows, err := manager.Acquire(context.Background(), "phone-a", "pane-1", 84, 26); err != nil || rows != 26 {
		t.Fatalf("row change Acquire() rows = %d, %v, want 26", rows, err)
	}
	if !manager.ResizedWithin("pane-1", time.Second) {
		t.Fatal("row change did not open the settle window")
	}

	// Both row-leasing phones leave: the surviving width lease keeps its
	// narrower width while the pane gets its own height back.
	if err := manager.Release(context.Background(), "phone-a", "pane-1"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Release(context.Background(), "phone-b", "pane-1"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(ReleaseGrace)
	if err := manager.SweepExpired(context.Background()); err != nil {
		t.Fatalf("SweepExpired() error = %v", err)
	}
	if got := runner.sizes["/dev/pts/15"]; got.rows != 64 || got.columns != 90 {
		t.Fatalf("size after row leases lapsed = %+v, want 90x64", got)
	}
	if got, ok := manager.ActiveRows("pane-1"); !ok || got != 64 {
		t.Fatalf("ActiveRows() with width-only lease = %d, %v, want 64, true", got, ok)
	}

	// Last client gone: full restore. Same-size lease additions above did not
	// reassert the active row constraint; only real row changes reached stty.
	if err := manager.Release(context.Background(), "desk-c", "pane-1"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(ReleaseGrace)
	if err := manager.SweepExpired(context.Background()); err != nil {
		t.Fatalf("SweepExpired() error = %v", err)
	}
	if got := runner.sizes["/dev/pts/15"]; got.rows != 64 || got.columns != 180 {
		t.Fatalf("restored size = %+v, want 180x64", got)
	}
	if !slices.Equal(runner.setRowValues, []int{30, 26, 64}) {
		t.Fatalf("applied rows = %v, want [30 26 64]", runner.setRowValues)
	}
	if len(manager.panes) != 0 {
		t.Fatalf("tracked panes = %d after final release", len(manager.panes))
	}
}

func TestSttyDeviceFlagIsPlatformAppropriate(t *testing.T) {
	if flag, err := sttyDeviceFlag("linux"); err != nil || flag != "-F" {
		t.Fatalf("linux flag = %q, %v", flag, err)
	}
	if flag, err := sttyDeviceFlag("darwin"); err != nil || flag != "-f" {
		t.Fatalf("darwin flag = %q, %v", flag, err)
	}
}
