package ownedproc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

type Native struct{}

func processFromKinfo(k unix.KinfoProc) Process {
	birth := ""
	if k.Proc.P_starttime.Sec > 0 {
		birth = fmt.Sprintf("%d:%d", k.Proc.P_starttime.Sec, k.Proc.P_starttime.Usec)
	}
	return Process{PID: int(k.Proc.P_pid), Parent: int(k.Eproc.Ppid), Birth: birth, Exited: k.Proc.P_stat == 5}
}

func (Native) Lookup(ctx context.Context, pid int) (Process, error) {
	if err := ctx.Err(); err != nil {
		return Process{}, err
	}
	data, err := unix.SysctlRaw("kern.proc.pid", pid)
	if errors.Is(err, unix.ESRCH) || errors.Is(err, unix.ENOENT) {
		return Process{}, ErrAbsent
	}
	if err != nil {
		return Process{}, err
	}
	if err := ctx.Err(); err != nil {
		return Process{}, err
	}
	if len(data) == 0 {
		return Process{}, ErrAbsent
	}
	if len(data) != unix.SizeofKinfoProc {
		return Process{}, ErrIncomplete
	}
	return decodeKinfo(data), nil
}

func (Native) Table(ctx context.Context) ([]Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := unix.SysctlRaw("kern.proc.uid", os.Getuid())
	if err != nil {
		return nil, err
	}
	if len(data)%unix.SizeofKinfoProc != 0 || len(data)/unix.SizeofKinfoProc > MaxTable {
		return nil, ErrIncomplete
	}
	result := make([]Process, 0, len(data)/unix.SizeofKinfoProc)
	for offset := 0; offset < len(data); offset += unix.SizeofKinfoProc {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		result = append(result, decodeKinfo(data[offset:offset+unix.SizeofKinfoProc]))
	}
	return result, nil
}

func decodeKinfo(data []byte) Process {
	var k unix.KinfoProc
	copy(unsafe.Slice((*byte)(unsafe.Pointer(&k)), unix.SizeofKinfoProc), data)
	return processFromKinfo(k)
}

func (Native) Signal(pid int, force bool) error {
	if pid <= 0 {
		return ErrIncomplete
	}
	signal := syscall.SIGTERM
	if force {
		signal = syscall.SIGKILL
	}
	return syscall.Kill(pid, signal)
}
