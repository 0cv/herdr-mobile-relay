package app

import (
	"context"
	"errors"
	"golang.org/x/sys/unix"
)

func hermesProcessStart(ctx context.Context, pid int) (float64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	p, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, err
	}
	if p == nil || int(p.Proc.P_pid) != pid || p.Proc.P_stat == 5 || p.Proc.P_starttime.Sec <= 0 {
		return 0, errors.New("process start unavailable")
	}
	return float64(p.Proc.P_starttime.Sec) + float64(p.Proc.P_starttime.Usec)/1e6, nil
}
