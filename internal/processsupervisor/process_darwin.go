//go:build darwin

package processsupervisor

import (
	"os"
	"syscall"
)

func anchorProcessAlive(process *os.Process) bool {
	return process != nil && process.Signal(syscall.Signal(0)) == nil
}
