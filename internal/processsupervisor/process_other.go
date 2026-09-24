//go:build !linux && !darwin

package processsupervisor

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func supportedPlatform() bool { return false }

func processGroupID(int) (int, error) { return 0, errors.New("process groups unsupported") }

func anchorProcessAlive(*os.Process) bool { return false }

func signalOwnedGroup(int, syscall.Signal) error {
	return errors.New("process groups unsupported")
}

func startAnchor(runConfig, string, time.Duration) (*anchor, error) {
	return nil, errors.New("process supervision is supported only on Linux and macOS")
}

func startTarget([]string, int, Streams) (*exec.Cmd, error) {
	return nil, errors.New("process supervision is supported only on Linux and macOS")
}

func processExitCode(error) (int, error) {
	return 1, errors.New("process exit status is unavailable on this platform")
}

func expectedFinalGroupKill(error) bool { return false }

func RunAnchor() (int, error) {
	return 1, errors.New("private process anchor is unsupported on this platform")
}
