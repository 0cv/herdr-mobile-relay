// Package processgroup runs commands in Unix process groups with bounded cancellation.
package processgroup

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"syscall"
	"time"
)

const (
	termGrace = 2 * time.Second
	waitDelay = 4 * time.Second
)

// Options controls the caller-specific cancellation policy.
type Options struct {
	PostKillWait         time.Duration
	ReturnOutputOnCancel bool
}

// Run collects stdout followed by stderr, terminating the process group on cancellation.
func Run(ctx context.Context, command *exec.Cmd, options Options) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.WaitDelay = waitDelay
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		return nil, err
	}

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- command.Wait()
	}()
	select {
	case err := <-waitCh:
		return combinedCommandOutput(stdout, stderr), err
	case <-ctx.Done():
		// Wait joins os/exec's stream-copy goroutines. If it has not completed,
		// those goroutines may still write, so neither buffer may be read.
		if terminateProcessGroup(command.Process.Pid, waitCh, options.PostKillWait) && options.ReturnOutputOnCancel {
			return combinedCommandOutput(stdout, stderr), ctx.Err()
		}
		return nil, ctx.Err()
	}
}

func combinedCommandOutput(stdout, stderr *bytes.Buffer) []byte {
	output := make([]byte, 0, stdout.Len()+stderr.Len())
	output = append(output, stdout.Bytes()...)
	return append(output, stderr.Bytes()...)
}

func terminateProcessGroup(pgid int, waitCh <-chan error, postKillWait time.Duration) bool {
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	timer := time.NewTimer(termGrace)
	defer timer.Stop()
	waitCompleted := false
	select {
	case <-waitCh:
		waitCompleted = true
		if !processGroupAlive(pgid) {
			return true
		}
		<-timer.C
	case <-timer.C:
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)

	if !waitCompleted {
		select {
		case <-waitCh:
			waitCompleted = true
		case <-time.After(postKillWait):
		}
	}
	deadline := time.Now().Add(termGrace)
	for processGroupAlive(pgid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	return waitCompleted
}

func processGroupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
