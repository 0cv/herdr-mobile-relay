//go:build linux

package processsupervisor

import (
	"syscall"
	"testing"
)

var testSubreaperEnabled bool

func init() {
	// Adopt orphaned fixture descendants so tests can reap them and prove their
	// PIDs are gone instead of confusing an unreaped zombie with a live child.
	_, _, errno := syscall.Syscall6(syscall.SYS_PRCTL, 36, 1, 0, 0, 0, 0)
	testSubreaperEnabled = errno == 0
}

func TestFixtureSubreaperEnabled(t *testing.T) {
	if !testSubreaperEnabled {
		t.Fatal("could not enable fixture-only child subreaping")
	}
}

func processRetired(pid int) bool {
	var status syscall.WaitStatus
	waited, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
	if waited == pid {
		return true
	}
	if err == nil {
		return false
	}
	if err == syscall.ECHILD {
		return syscall.Kill(pid, 0) == syscall.ESRCH
	}
	return false
}
