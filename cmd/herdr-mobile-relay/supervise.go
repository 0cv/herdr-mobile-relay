package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/processsupervisor"
)

type superviseOptions struct {
	grace   time.Duration
	command []string
}

func parseSuperviseArgs(args []string) (superviseOptions, error) {
	options := superviseOptions{grace: 5 * time.Second}
	index := 0
	if len(args) > 0 && args[0] == "--grace" {
		if len(args) < 2 {
			return superviseOptions{}, errors.New("--grace requires a duration")
		}
		grace, err := time.ParseDuration(args[1])
		if err != nil || grace < 100*time.Millisecond || grace > 30*time.Second {
			return superviseOptions{}, errors.New("--grace must be between 100ms and 30s")
		}
		options.grace = grace
		index = 2
	}
	if index >= len(args) || args[index] != "--" {
		if index < len(args) && strings.HasPrefix(args[index], "-") {
			return superviseOptions{}, fmt.Errorf("unknown or duplicate supervise flag %q", args[index])
		}
		return superviseOptions{}, errors.New("usage: herdr-mobile-relay supervise [--grace 5s] -- COMMAND [ARG...]")
	}
	options.command = append([]string(nil), args[index+1:]...)
	if len(options.command) == 0 || options.command[0] == "" {
		return superviseOptions{}, errors.New("supervise requires a non-empty command after --")
	}
	return options, nil
}

func runSupervise(args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	options, err := parseSuperviseArgs(args)
	if err != nil {
		return 2, err
	}
	if !processsupervisor.Supported() {
		return 1, errors.New("process supervision is supported only on Linux and macOS")
	}
	return processsupervisor.Run(options.command, options.grace, processsupervisor.Streams{
		Stdin: stdin, Stdout: stdout, Stderr: stderr,
	})
}
