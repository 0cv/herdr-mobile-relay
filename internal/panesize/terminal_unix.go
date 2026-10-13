//go:build darwin || linux

package panesize

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// platformSupported reports that this build can resize a PTY directly.
const platformSupported = true

type ioctlTerminalOps struct{}

func (ioctlTerminalOps) Open(path string) (Terminal, error) {
	for {
		fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOCTTY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", path, err)
		}
		return &ioctlTerminal{fd: fd}, nil
	}
}

type ioctlTerminal struct {
	fd int
}

func (t *ioctlTerminal) Device() (uint64, bool, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(t.fd, &stat); err != nil {
		return 0, false, err
	}
	return deviceNumber(stat), uint32(stat.Mode)&unix.S_IFMT == unix.S_IFCHR, nil
}

func (t *ioctlTerminal) Size() (Winsize, error) {
	for {
		size, err := unix.IoctlGetWinsize(t.fd, unix.TIOCGWINSZ)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return Winsize{}, err
		}
		return Winsize{Rows: size.Row, Columns: size.Col, XPixel: size.Xpixel, YPixel: size.Ypixel}, nil
	}
}

func (t *ioctlTerminal) SetSize(size Winsize) error {
	value := unix.Winsize{Row: size.Rows, Col: size.Columns, Xpixel: size.XPixel, Ypixel: size.YPixel}
	for {
		err := unix.IoctlSetWinsize(t.fd, unix.TIOCSWINSZ, &value)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return err
	}
}

func (t *ioctlTerminal) Close() error {
	return unix.Close(t.fd)
}

// deviceNumber widens st_rdev (int32 on darwin, uint64 on linux) the same way
// at capture and at every comparison.
func deviceNumber(stat unix.Stat_t) uint64 {
	return uint64(stat.Rdev)
}
