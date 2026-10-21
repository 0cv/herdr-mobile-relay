package panesize

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/0cv/herdr-mobile-relay/internal/childenv"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

// Winsize mirrors struct winsize. The pixel fields are carried through every
// write unchanged; only rows and columns are ever chosen by the manager.
type Winsize struct {
	Rows    uint16
	Columns uint16
	XPixel  uint16
	YPixel  uint16
}

// Terminal is one open descriptor on a pane's TTY device. Every operation
// acts on the held descriptor, never on the pathname it was opened from.
type Terminal interface {
	// Device reports fstat of the held descriptor: the device number and
	// whether it is a character device.
	Device() (rdev uint64, charDevice bool, err error)
	Size() (Winsize, error)
	SetSize(Winsize) error
	Close() error
}

// TerminalOps opens TTY devices. The production implementation opens with
// O_RDONLY|O_NOCTTY|O_NONBLOCK|O_CLOEXEC: it never becomes the controlling
// terminal, never blocks on a hung-up device, and never leaks into children.
type TerminalOps interface {
	Open(path string) (Terminal, error)
}

// TTYResolver reports the controlling terminal of a process as a /dev path.
type TTYResolver interface {
	TTYPath(ctx context.Context, pid int) (string, error)
}

// Provider is the fresh Herdr lookup surface. Each call must reach the
// running server: identity checks never reuse an earlier answer.
type Provider interface {
	PaneGet(ctx context.Context, paneID string) (*herdr.Pane, error)
	PaneProcessInfoFresh(ctx context.Context, paneID string) (*herdr.PaneProcessInfo, error)
}

type psTTYResolver struct{}

func (psTTYResolver) TTYPath(ctx context.Context, pid int) (string, error) {
	if pid <= 0 {
		return "", ErrTTYUnavailable
	}
	output, err := childenv.CommandContext(ctx, "ps", "-o", "tty=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", ErrTTYUnavailable
	}
	return ttyPath(output)
}

// ttyPath turns `ps -o tty=` output ("pts/7" on Linux, "ttys003" on macOS)
// into a /dev path, refusing anything that could escape /dev.
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

// foregroundPID is the fallback identity process when Herdr reports no shell:
// the foreground process group leader, or any foreground process.
func foregroundPID(info *herdr.PaneProcessInfo) (int, bool) {
	if info == nil || info.ForegroundProcessGroupID <= 0 || len(info.ForegroundProcesses) == 0 {
		return 0, false
	}
	for _, process := range info.ForegroundProcesses {
		if process.PID == info.ForegroundProcessGroupID {
			return process.PID, true
		}
	}
	for _, process := range info.ForegroundProcesses {
		if process.PID > 0 {
			return process.PID, true
		}
	}
	return 0, false
}
