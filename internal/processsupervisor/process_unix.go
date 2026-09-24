//go:build linux || darwin

package processsupervisor

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func supportedPlatform() bool { return true }

func processGroupID(pid int) (int, error) { return syscall.Getpgid(pid) }

func signalOwnedGroup(anchorPID int, signal syscall.Signal) error {
	// The anchor is the group leader and stays alive through this syscall. Its
	// unreaped process therefore pins this PGID generation against reuse.
	return syscall.Kill(-anchorPID, signal)
}

func startAnchor(config runConfig, token string, grace time.Duration) (*anchor, error) {
	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create anchor control pipe: %w", err)
	}
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		_ = controlRead.Close()
		_ = controlWrite.Close()
		return nil, fmt.Errorf("create anchor readiness pipe: %w", err)
	}

	args := config.anchorArgs
	if len(args) == 0 {
		args = []string{anchorSubcommand}
	}
	cmd := exec.Command(config.selfPath, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.ExtraFiles = []*os.File{controlRead, readyWrite}
	cmd.Env = []string{}
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		_ = controlRead.Close()
		_ = controlWrite.Close()
		_ = readyRead.Close()
		_ = readyWrite.Close()
		return nil, fmt.Errorf("start private process anchor: %w", err)
	}
	_ = controlRead.Close()
	_ = readyWrite.Close()

	owned := &anchor{
		cmd:      cmd,
		control:  controlWrite,
		ready:    readyRead,
		statusCh: readAnchorRecords(readyRead),
	}
	if _, err := fmt.Fprintf(controlWrite, "START %s %d\n", token, grace.Nanoseconds()); err != nil {
		_ = controlWrite.Close()
		owned.control = nil
		return owned, fmt.Errorf("initialize private process anchor: %w", err)
	}
	return owned, nil
}

func startTarget(command []string, anchorPID int, streams Streams) (*exec.Cmd, error) {
	cmd := exec.Command(command[0], command[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: anchorPID}
	cmd.Stdin = streams.Stdin
	cmd.Stdout = streams.Stdout
	cmd.Stderr = streams.Stderr
	// Non-file streams make os/exec copy through pipes. Bound Wait after the
	// target exits so descendants retaining those descriptors cannot prevent
	// Run from starting process-group cleanup.
	cmd.WaitDelay = targetStreamWaitDelay
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

func processExitCode(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return 1, fmt.Errorf("wait for supervised command: %w", err)
	}
	if code := exitErr.ExitCode(); code >= 0 {
		return code, nil
	}
	if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal()), nil
	}
	return 1, fmt.Errorf("supervised command ended without an exit status: %w", err)
}

func expectedFinalGroupKill(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && status.Signal() == syscall.SIGKILL
}

// RunAnchor is the hidden command entry point. A caller must supply the two
// private pipes and start this process as its own group leader; no caller PID
// or process-group argument is accepted.
func RunAnchor() (int, error) {
	control := os.NewFile(3, "private-control")
	ready := os.NewFile(4, "private-readiness")
	if control == nil || ready == nil {
		return 1, errors.New("private anchor descriptors are unavailable")
	}
	defer control.Close()
	defer ready.Close()
	controlInfo, controlErr := control.Stat()
	readyInfo, readyErr := ready.Stat()
	if controlErr != nil || readyErr != nil || controlInfo.Mode()&os.ModeNamedPipe == 0 || readyInfo.Mode()&os.ModeNamedPipe == 0 {
		return 1, errors.New("private anchor descriptors are not pipes")
	}

	pid := os.Getpid()
	pgid, err := processGroupID(pid)
	if err != nil || pgid != pid {
		return 1, errors.New("private anchor is not its own process-group leader")
	}
	signal.Ignore(os.Interrupt, syscall.SIGTERM)
	reader := bufio.NewReaderSize(control, maxProtocolRecord)
	startLine, startErr := readRecord(reader, maxProtocolRecord)
	if startErr != nil {
		return 1, anchorCleanup(ready, syscall.SIGTERM, defaultGrace)
	}
	fields := strings.Fields(startLine)
	if len(fields) != 3 || fields[0] != "START" || len(fields[1]) != 48 {
		return 1, anchorCleanup(ready, syscall.SIGTERM, defaultGrace)
	}
	if _, err := hex.DecodeString(fields[1]); err != nil {
		return 1, anchorCleanup(ready, syscall.SIGTERM, defaultGrace)
	}
	graceNanos, err := strconv.ParseInt(fields[2], 10, 64)
	grace := time.Duration(graceNanos)
	if err != nil || grace < minGrace || grace > maxGrace {
		return 1, anchorCleanup(ready, syscall.SIGTERM, defaultGrace)
	}
	if _, err := fmt.Fprintf(ready, "READY %s %d %d %d\n", fields[1], pid, os.Getppid(), pgid); err != nil {
		return 1, anchorCleanup(ready, syscall.SIGTERM, grace)
	}

	stopLine, stopErr := readRecord(reader, maxProtocolRecord)
	cleanupSignal := syscall.SIGTERM
	if stopErr == nil {
		stopFields := strings.Fields(stopLine)
		if len(stopFields) == 2 && stopFields[0] == "STOP" && stopFields[1] == "INT" {
			cleanupSignal = syscall.SIGINT
		} else if len(stopFields) != 2 || stopFields[0] != "STOP" || stopFields[1] != "TERM" {
			cleanupSignal = syscall.SIGTERM
		}
	}
	return 0, anchorCleanup(ready, cleanupSignal, grace)
}

func anchorCleanup(ready *os.File, graceful syscall.Signal, grace time.Duration) error {
	pid := os.Getpid()
	if err := signalOwnedGroup(pid, graceful); err != nil {
		writeAnchorFailure(ready, fmt.Errorf("send %s to pinned process group: %w", graceful, err))
	}
	timer := time.NewTimer(grace)
	<-timer.C
	if err := signalOwnedGroup(pid, syscall.SIGKILL); err != nil {
		writeAnchorFailure(ready, fmt.Errorf("send SIGKILL to pinned process group: %w", err))
		self, ownErr := os.FindProcess(pid)
		if ownErr == nil {
			ownErr = self.Kill()
		}
		if ownErr != nil {
			return fmt.Errorf("group SIGKILL failed (%v), and anchor self-kill failed: %w", err, ownErr)
		}
		select {}
	}
	// Do not return normally: the anchor must not release its pinned PGID after
	// issuing the final signal, even if signal delivery is not instantaneous.
	select {}
}

func writeAnchorFailure(ready *os.File, err error) {
	message := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == 0 {
			return ' '
		}
		return r
	}, err.Error())
	if len(message) > maxProtocolRecord-6 {
		message = message[:maxProtocolRecord-6]
	}
	_, _ = fmt.Fprintf(ready, "FAIL %s\n", message)
}
