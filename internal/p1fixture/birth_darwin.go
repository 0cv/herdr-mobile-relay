package p1fixture

import (
	"fmt"
	"golang.org/x/sys/unix"
)

func childBirth(pid int) (string, error) {
	process, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", err
	}
	if int(process.Proc.P_pid) != pid || process.Proc.P_starttime.Sec <= 0 {
		return "", ErrContract
	}
	return fmt.Sprintf("%d:%d", process.Proc.P_starttime.Sec, process.Proc.P_starttime.Usec), nil
}
