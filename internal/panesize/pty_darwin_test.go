//go:build darwin

package panesize

import (
	"bytes"
	"os"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// openPTY opens a real pseudo-terminal pair the way posix_openpt, grantpt,
// unlockpt and ptsname do on macOS.
func openPTY(t *testing.T) (*os.File, string) {
	t.Helper()
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Skipf("cannot open /dev/ptmx in this environment: %v", err)
	}
	master := os.NewFile(uintptr(fd), "/dev/ptmx")
	if err := unix.IoctlSetInt(fd, unix.TIOCPTYGRANT, 0); err != nil {
		master.Close()
		t.Fatalf("TIOCPTYGRANT: %v", err)
	}
	if err := unix.IoctlSetInt(fd, unix.TIOCPTYUNLK, 0); err != nil {
		master.Close()
		t.Fatalf("TIOCPTYUNLK: %v", err)
	}
	var name [128]byte
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(unix.TIOCPTYGNAME), uintptr(unsafe.Pointer(&name[0]))); errno != 0 {
		master.Close()
		t.Fatalf("TIOCPTYGNAME: %v", errno)
	}
	end := bytes.IndexByte(name[:], 0)
	if end <= 0 {
		master.Close()
		t.Fatal("TIOCPTYGNAME returned an empty name")
	}
	return master, string(name[:end])
}
