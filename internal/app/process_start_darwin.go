package app

import (
	"context"
	"errors"
	"time"

	"golang.org/x/sys/unix"
)

func processStartTime(ctx context.Context, pid int) (time.Time, error) {
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	p, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return time.Time{}, err
	}
	if p == nil || int(p.Proc.P_pid) != pid || p.Proc.P_stat == 5 || p.Proc.P_starttime.Sec <= 0 || p.Proc.P_starttime.Usec < 0 || p.Proc.P_starttime.Usec >= 1000000 {
		return time.Time{}, errors.New("process start unavailable")
	}
	return time.Unix(p.Proc.P_starttime.Sec, int64(p.Proc.P_starttime.Usec)*1000), nil
}
