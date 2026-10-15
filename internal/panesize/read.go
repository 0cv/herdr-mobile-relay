package panesize

import (
	"context"
	"errors"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const MaxCellDimension = 65535

type Grid struct {
	TTY     string
	Rows    int
	Columns int
}

type CommandMeasurement struct {
	Name     string
	Duration time.Duration
	Failed   bool
}

type observedRunner struct {
	runner  CommandRunner
	observe func(context.Context, CommandMeasurement)
}

func (r observedRunner) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	started := time.Now()
	out, err := r.runner.Output(ctx, name, args...)
	r.observe(ctx, CommandMeasurement{Name: name, Duration: time.Since(started), Failed: err != nil})
	return out, err
}

type Reader struct {
	runner  CommandRunner
	goos    string
	Observe func(context.Context, CommandMeasurement)
}

func NewReader(runner CommandRunner, goos string) *Reader {
	if runner == nil {
		runner = execCommandRunner{}
	}
	if goos == "" {
		goos = runtime.GOOS
	}
	return &Reader{runner: runner, goos: goos}
}

func (r *Reader) Read(ctx context.Context, pid int) (Grid, error) {
	if pid <= 0 {
		return Grid{}, ErrProcessUnavailable
	}
	if err := ctx.Err(); err != nil {
		return Grid{}, err
	}
	if _, err := sttyDeviceFlag(r.goos); err != nil {
		return Grid{}, err
	}
	runner := r.runner
	if r.Observe != nil {
		runner = observedRunner{runner: runner, observe: r.Observe}
	}
	output, err := runner.Output(ctx, "ps", "-o", "tty=", "-p", strconv.Itoa(pid))
	if err != nil {
		return Grid{}, errors.Join(ErrTTYUnavailable, err, ctx.Err())
	}
	if err := ctx.Err(); err != nil {
		return Grid{}, err
	}
	tty, err := ttyPath(output)
	if err != nil {
		return Grid{}, err
	}
	size, err := readTTYSize(ctx, runner, r.goos, tty)
	if err != nil {
		return Grid{}, err
	}
	return Grid{TTY: tty, Rows: size.rows, Columns: size.columns}, nil
}

func readTTYSize(ctx context.Context, runner CommandRunner, goos, tty string) (terminalSize, error) {
	if err := ctx.Err(); err != nil {
		return terminalSize{}, err
	}
	flag, err := sttyDeviceFlag(goos)
	if err != nil {
		return terminalSize{}, err
	}
	output, err := runner.Output(ctx, "stty", flag, tty, "size")
	if err != nil {
		return terminalSize{}, errors.Join(ErrSizeUnavailable, err, ctx.Err())
	}
	if err := ctx.Err(); err != nil {
		return terminalSize{}, err
	}
	fields := strings.Fields(string(output))
	if len(fields) != 2 {
		return terminalSize{}, ErrSizeUnavailable
	}
	dimension := func(value string) (int, error) {
		if strings.IndexFunc(value, func(c rune) bool { return c < '0' || c > '9' }) != -1 {
			return 0, ErrSizeUnavailable
		}
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > MaxCellDimension {
			return 0, ErrSizeUnavailable
		}
		return n, nil
	}
	rows, rowErr := dimension(fields[0])
	columns, columnErr := dimension(fields[1])
	if rowErr != nil || columnErr != nil {
		return terminalSize{}, ErrSizeUnavailable
	}
	return terminalSize{rows: rows, columns: columns}, nil
}
