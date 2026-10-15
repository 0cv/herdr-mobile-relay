package ownedproc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
)

type Native struct{}

func (Native) Lookup(ctx context.Context, pid int) (Process, error) {
	if err := ctx.Err(); err != nil {
		return Process{}, err
	}
	file, err := os.Open(fmt.Sprintf("/proc/%d/stat", pid))
	if errors.Is(err, os.ErrNotExist) {
		return Process{}, ErrAbsent
	}
	if err != nil {
		return Process{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 16385))
	if err != nil {
		return Process{}, err
	}
	if len(data) > 16384 {
		return Process{}, ErrIncomplete
	}
	if err := ctx.Err(); err != nil {
		return Process{}, err
	}
	return parseStat(pid, string(data))
}

func parseStat(pid int, data string) (Process, error) {
	close := strings.LastIndex(data, ")")
	open := strings.Index(data, "(")
	if open < 1 || close <= open {
		return Process{}, ErrIncomplete
	}
	recorded, err := strconv.Atoi(strings.TrimSpace(data[:open]))
	if err != nil || recorded != pid {
		return Process{}, ErrIncomplete
	}
	fields := strings.Fields(data[close+1:])
	if len(fields) < 20 {
		return Process{}, ErrIncomplete
	}
	parent, err := strconv.Atoi(fields[1])
	if err != nil {
		return Process{}, err
	}
	birth, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || birth == 0 {
		return Process{}, ErrIncomplete
	}
	return Process{PID: pid, Parent: parent, Birth: strconv.FormatUint(birth, 10), Exited: fields[0] == "Z" || fields[0] == "X"}, nil
}

func (n Native) Table(ctx context.Context) ([]Process, error) {
	file, err := os.Open("/proc")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var result []Process
	count := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		names, err := file.Readdirnames(128)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		for _, name := range names {
			pid, conversion := strconv.Atoi(name)
			if conversion != nil {
				continue
			}
			count++
			if count > MaxTable {
				return nil, ErrIncomplete
			}
			row, lookup := n.Lookup(ctx, pid)
			if errors.Is(lookup, ErrAbsent) {
				continue
			}
			if lookup != nil {
				return nil, lookup
			}
			result = append(result, row)
		}
		if errors.Is(err, io.EOF) {
			return result, nil
		}
	}
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
