//go:build darwin || linux

package panesize

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"slices"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func masterSize(t *testing.T, master *os.File) Winsize {
	t.Helper()
	size, err := unix.IoctlGetWinsize(int(master.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		t.Fatalf("TIOCGWINSZ on master: %v", err)
	}
	return Winsize{Rows: size.Row, Columns: size.Col, XPixel: size.Xpixel, YPixel: size.Ypixel}
}

func setMasterSize(t *testing.T, master *os.File, size Winsize) {
	t.Helper()
	value := unix.Winsize{Row: size.Rows, Col: size.Columns, Xpixel: size.XPixel, Ypixel: size.YPixel}
	if err := unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, &value); err != nil {
		t.Fatalf("TIOCSWINSZ on master: %v", err)
	}
}

// holdSlave opens the slave side the way the pane's shell does. On macOS the
// master answers window-size ioctls only once the slave has been opened.
func holdSlave(t *testing.T, path string) {
	t.Helper()
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
}

func deviceOf(t *testing.T, path string) uint64 {
	t.Helper()
	var stat unix.Stat_t
	if err := unix.Stat(path, &stat); err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return deviceNumber(stat)
}

func TestPlatformManagerIsAvailable(t *testing.T) {
	if !NewManager(&fakeHerdr{}, nil).Available() {
		t.Fatal("darwin/linux manager reports itself unavailable")
	}
}

func TestIoctlTerminalReadsAndWritesWinsize(t *testing.T) {
	master, slavePath := openPTY(t)
	defer master.Close()
	holdSlave(t, slavePath)
	setMasterSize(t, master, Winsize{Rows: 24, Columns: 80, XPixel: 640, YPixel: 480})

	terminal, err := ioctlTerminalOps{}.Open(slavePath)
	if err != nil {
		t.Fatalf("Open(%s): %v", slavePath, err)
	}
	defer terminal.Close()
	rdev, character, err := terminal.Device()
	if err != nil || !character || rdev != deviceOf(t, slavePath) {
		t.Fatalf("Device() = %#x, %v, %v; stat rdev %#x", rdev, character, err, deviceOf(t, slavePath))
	}
	size, err := terminal.Size()
	if err != nil || size != (Winsize{Rows: 24, Columns: 80, XPixel: 640, YPixel: 480}) {
		t.Fatalf("Size() = %+v, %v", size, err)
	}
	want := Winsize{Rows: 30, Columns: 100, XPixel: 640, YPixel: 480}
	if err := terminal.SetSize(want); err != nil {
		t.Fatalf("SetSize(): %v", err)
	}
	if got := masterSize(t, master); got != want {
		t.Fatalf("master sees %+v, want %+v", got, want)
	}
}

func TestIoctlTerminalOpenOfMissingDeviceIsNotExist(t *testing.T) {
	_, err := ioctlTerminalOps{}.Open("/dev/herdr-mobile-relay-no-such-tty")
	if !errors.Is(err, unix.ENOENT) {
		t.Fatalf("Open() error = %v, want ENOENT", err)
	}
}

// The real implementation behind the identity boundary: an acquisition binds
// to the PTY's device number, and a renewal whose cached path now leads to a
// different device is refused without writing either terminal.
func TestRealPTYDeviceMismatchIsRefused(t *testing.T) {
	master, slavePath := openPTY(t)
	defer master.Close()
	other, otherPath := openPTY(t)
	defer other.Close()
	holdSlave(t, slavePath)
	holdSlave(t, otherPath)
	setMasterSize(t, master, Winsize{Rows: 40, Columns: 120, XPixel: 960, YPixel: 640})
	setMasterSize(t, other, Winsize{Rows: 24, Columns: 80})

	clock := &fakeClock{now: time.Unix(10_000, 0)}
	provider := &fakeHerdr{
		clock: clock,
		panes: map[string]*fakePane{"pane-1": {terminalID: "terminal-1", shellPID: 321}},
		fail:  map[string]error{}, block: map[string]bool{},
	}
	ttys := &fakeTTYs{byPID: map[int]string{321: slavePath}}
	logs := &logRecorder{}
	m := newManager(provider, ttys, ioctlTerminalOps{}, LeaseTTL, ReleaseGrace, clock.Now, slog.New(logs))

	if columns, rows, err := m.Acquire(context.Background(), "client-a", "pane-1", 84, 0); err != nil || columns != 84 || rows != 40 {
		t.Fatalf("Acquire() = %d, %d, %v", columns, rows, err)
	}
	if got := masterSize(t, master); got != (Winsize{Rows: 40, Columns: 84, XPixel: 960, YPixel: 640}) {
		t.Fatalf("leased size = %+v", got)
	}
	m.mu.Lock()
	captured := m.panes["pane-1"].identity.rdev
	// The cached path now leads to another PTY, and Herdr and ps agree
	// with it: only the device number tells the two terminals apart.
	m.panes["pane-1"].identity.ttyPath = otherPath
	m.mu.Unlock()
	if captured != deviceOf(t, slavePath) {
		t.Fatalf("captured rdev %#x, want %#x", captured, deviceOf(t, slavePath))
	}
	ttys.set(321, otherPath)

	if _, _, err := m.Acquire(context.Background(), "client-a", "pane-1", 60, 0); !errors.Is(err, ErrTerminalGone) {
		t.Fatalf("renewal error = %v, want terminal gone", err)
	}
	if got := masterSize(t, other); got != (Winsize{Rows: 24, Columns: 80}) {
		t.Fatalf("mismatched PTY was written: %+v", got)
	}
	if got := masterSize(t, master); got.Columns != 84 {
		t.Fatalf("original PTY was written: %+v", got)
	}
	m.mu.Lock()
	tracked := len(m.panes)
	m.mu.Unlock()
	if tracked != 0 || logs.count(slog.LevelInfo, "dropped") != 1 {
		t.Fatalf("tracked=%d info=%d", tracked, logs.count(slog.LevelInfo, "dropped"))
	}
}

// Empirical check of the claim openValidated relies on: while a descriptor
// on a PTY's slave side stays open, closing the master does not hand that
// PTY's name to a new pseudo-terminal.
func TestHeldSlaveDescriptorPinsPTYName(t *testing.T) {
	master, slavePath := openPTY(t)
	held, err := ioctlTerminalOps{}.Open(slavePath)
	if err != nil {
		master.Close()
		t.Fatalf("Open(%s): %v", slavePath, err)
	}
	if err := master.Close(); err != nil {
		t.Fatal(err)
	}
	var allocated []*os.File
	var names []string
	for range 8 {
		next, name := openPTY(t)
		allocated = append(allocated, next)
		names = append(names, name)
	}
	if slices.Contains(names, slavePath) {
		t.Fatalf("%s was reallocated while a descriptor on it was held: %v", slavePath, names)
	}
	_ = held.Close()
	for _, file := range allocated {
		_ = file.Close()
	}
	// Informational only: after the last close the name may be reused.
	reused, name := openPTY(t)
	defer reused.Close()
	t.Logf("held %s; allocations while held: %v; next after release: %s", slavePath, names, name)
}
