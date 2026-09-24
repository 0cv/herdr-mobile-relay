//go:build darwin

package processsupervisor

import "syscall"

func processRetired(pid int) bool {
	return syscall.Kill(pid, 0) == syscall.ESRCH
}
