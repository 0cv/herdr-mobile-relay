//go:build linux

package processsupervisor

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func anchorProcessAlive(process *os.Process) bool {
	if process == nil {
		return false
	}
	// WNOWAIT observes an exited direct child without reaping it. Keeping the
	// child unreaped pins its PID until process-group cleanup has completed.
	var info unix.Siginfo
	if err := unix.Waitid(unix.P_PID, process.Pid, &info, unix.WEXITED|unix.WNOHANG|unix.WNOWAIT, nil); err != nil {
		return false
	}
	if info.Signo != 0 {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}
