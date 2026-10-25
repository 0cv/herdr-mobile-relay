package app

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func kimiProcessStart(pid int) (string, error) {
	process, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", err
	}
	if process == nil || int(process.Proc.P_pid) != pid || process.Proc.P_starttime.Sec <= 0 || process.Proc.P_starttime.Usec < 0 || process.Proc.P_starttime.Usec >= 1000000 {
		return "", errors.New("Kimi process start time unavailable")
	}
	return fmt.Sprintf("%d:%06d", process.Proc.P_starttime.Sec, process.Proc.P_starttime.Usec), nil
}
