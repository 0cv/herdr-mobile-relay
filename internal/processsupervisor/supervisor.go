// Package processsupervisor runs a foreground command in an owned process group
// pinned by a private anchor process. The anchor remains the group leader until
// it sends the group's final SIGKILL, preventing PGID reuse between validation
// and cleanup.
package processsupervisor

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	defaultGrace          = 5 * time.Second
	minGrace              = 100 * time.Millisecond
	maxGrace              = 30 * time.Second
	readinessTimeout      = 3 * time.Second
	cleanupWaitAllowance  = 5 * time.Second
	targetStreamWaitDelay = time.Second
	anchorSubcommand      = "__private-process-anchor"
	maxProtocolRecord     = 256
)

// Streams are connected to the target's standard handles. Non-*os.File output
// writers use os/exec copier pipes; the supervisor bounds post-exit copier
// waits to targetStreamWaitDelay, so descendants retaining stdout or stderr
// cannot postpone group cleanup indefinitely. Such descendants may lose
// trailing output after the delay. Caller-provided Read and Write methods must
// not block forever because arbitrary io.Reader/io.Writer calls are not
// interruptible by the supervisor.
type Streams struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

type runConfig struct {
	selfPath   string
	anchorArgs []string
}

type anchor struct {
	cmd      *exec.Cmd
	control  *os.File
	ready    *os.File
	statusCh <-chan string
	waitCh   <-chan error
}

// Supported reports whether this build has process-group supervision support.
func Supported() bool { return supportedPlatform() }

// Run starts command without shell evaluation and returns its exit code after
// the owned process group has been retired. Internal/startup failures return an
// error; callers should map those errors to exit status 1.
func Run(command []string, grace time.Duration, streams Streams) (int, error) {
	selfPath, err := os.Executable()
	if err != nil {
		return 1, fmt.Errorf("locate supervisor executable: %w", err)
	}
	return run(command, grace, streams, runConfig{selfPath: selfPath})
}

func run(command []string, grace time.Duration, streams Streams, config runConfig) (int, error) {
	if len(command) == 0 || command[0] == "" {
		return 2, errors.New("supervise requires a command")
	}
	if grace < minGrace || grace > maxGrace {
		return 2, fmt.Errorf("grace must be between %s and %s", minGrace, maxGrace)
	}
	if !supportedPlatform() {
		return 1, errors.New("process supervision is supported only on Linux and macOS")
	}
	if config.selfPath == "" {
		return 1, errors.New("supervisor executable path is empty")
	}

	signals := make(chan os.Signal, 8)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	token, err := randomToken()
	if err != nil {
		return 1, fmt.Errorf("create private anchor token: %w", err)
	}
	owned, err := startAnchor(config, token, grace)
	if err != nil {
		if owned != nil {
			cleanupErr := retireAnchor(owned, "TERM", grace, nil)
			return 1, errors.Join(err, cleanupErr)
		}
		return 1, err
	}
	defer owned.closeFiles()

	readyLine, err := receiveReadiness(owned.statusCh)
	if err != nil {
		cleanupErr := retireAnchor(owned, "TERM", grace, nil)
		return 1, errors.Join(fmt.Errorf("anchor readiness failed: %w", err), cleanupErr)
	}
	if err := validateReadiness(readyLine, token, owned.cmd.Process.Pid); err != nil {
		cleanupErr := retireAnchor(owned, "TERM", grace, nil)
		return 1, errors.Join(fmt.Errorf("anchor readiness rejected: %w", err), cleanupErr)
	}
	if got, err := processGroupID(owned.cmd.Process.Pid); err != nil || got != owned.cmd.Process.Pid {
		if err == nil {
			err = fmt.Errorf("anchor process group is %d, expected %d", got, owned.cmd.Process.Pid)
		}
		cleanupErr := retireAnchor(owned, "TERM", grace, nil)
		return 1, errors.Join(fmt.Errorf("verify anchor group: %w", err), cleanupErr)
	}
	if !ensureAnchorAlive(owned) {
		cleanupErr := retireAnchor(owned, "TERM", grace, nil)
		return 1, errors.Join(errors.New("private anchor exited before target admission"), cleanupErr)
	}
	if chosen := takeSignal(signals); chosen != nil {
		cleanupErr := retireAnchor(owned, signalName(chosen), grace, nil)
		if cleanupErr != nil {
			return 1, cleanupErr
		}
		return signalExitCode(chosen), nil
	}

	target, err := startTarget(command, owned.cmd.Process.Pid, streams)
	if err != nil {
		cleanupErr := retireAnchor(owned, "TERM", grace, nil)
		return 1, errors.Join(fmt.Errorf("start supervised command: %w", err), cleanupErr)
	}
	if got, pgidErr := processGroupID(target.Process.Pid); pgidErr != nil || got != owned.cmd.Process.Pid {
		if pgidErr == nil {
			pgidErr = fmt.Errorf("target process group is %d, expected anchor %d", got, owned.cmd.Process.Pid)
		}
		killErr := target.Process.Kill()
		waitErr := target.Wait()
		cleanupErr := retireAnchor(owned, "TERM", grace, nil)
		return 1, errors.Join(fmt.Errorf("verify target group assignment: %w", pgidErr), killErr, waitErr, cleanupErr)
	}
	if !ensureAnchorAlive(owned) {
		killErr := target.Process.Kill()
		waitErr := target.Wait()
		cleanupErr := retireAnchor(owned, "TERM", grace, nil)
		return 1, errors.Join(errors.New("private anchor exited during target admission; group cleanup is unproven"), killErr, waitErr, cleanupErr)
	}

	targetWait := make(chan error, 1)
	go func() { targetWait <- target.Wait() }()

	var targetErr error
	var requestedSignal os.Signal
	select {
	case targetErr = <-targetWait:
		// The foreground command ending is itself a cleanup request. Descendants
		// cannot outlive a successful or failed direct child.
		targetWait = nil
	case requestedSignal = <-signals:
		// The first supervisor signal determines both group cleanup and status.
	}

	cleanupSignal := "TERM"
	if requestedSignal != nil {
		cleanupSignal = signalName(requestedSignal)
	}
	cleanupErr := retireAnchor(owned, cleanupSignal, grace, targetWait)
	if cleanupErr != nil {
		return 1, cleanupErr
	}
	if requestedSignal != nil {
		return signalExitCode(requestedSignal), nil
	}
	return commandExitCode(targetErr)
}

func randomToken() (string, error) {
	var bytes [24]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}

func validateReadiness(line, token string, pid int) error {
	fields := strings.Fields(line)
	if len(fields) != 5 || fields[0] != "READY" || fields[1] != token {
		return errors.New("malformed readiness record")
	}
	anchorPID, err := strconv.Atoi(fields[2])
	if err != nil || anchorPID != pid {
		return fmt.Errorf("readiness PID does not match started anchor %d", pid)
	}
	parentPID, err := strconv.Atoi(fields[3])
	if err != nil || parentPID != os.Getpid() {
		return fmt.Errorf("readiness parent PID does not match supervisor %d", os.Getpid())
	}
	groupID, err := strconv.Atoi(fields[4])
	if err != nil || groupID != pid {
		return fmt.Errorf("readiness process group does not match anchor %d", pid)
	}
	return nil
}

func receiveReadiness(status <-chan string) (string, error) {
	select {
	case line, ok := <-status:
		if !ok {
			return "", io.EOF
		}
		if strings.HasPrefix(line, "FAIL ") {
			return "", errors.New(strings.TrimPrefix(line, "FAIL "))
		}
		if strings.HasPrefix(line, "!ERROR ") {
			return "", errors.New(strings.TrimPrefix(line, "!ERROR "))
		}
		return line, nil
	case <-time.After(readinessTimeout):
		return "", errors.New("timed out waiting for anchor readiness")
	}
}

func readAnchorRecords(file *os.File) <-chan string {
	records := make(chan string, 4)
	go func() {
		defer close(records)
		for {
			line, err := readRecord(file, maxProtocolRecord)
			if err != nil {
				if !errors.Is(err, io.EOF) {
					records <- "!ERROR " + err.Error()
				}
				return
			}
			records <- line
		}
	}()
	return records
}

func readRecord(reader io.Reader, limit int) (string, error) {
	buffer := make([]byte, 0, limit)
	one := []byte{0}
	for len(buffer) < limit {
		n, err := reader.Read(one)
		if n > 0 {
			if one[0] == '\n' {
				return string(buffer), nil
			}
			buffer = append(buffer, one[0])
		}
		if err != nil {
			return "", err
		}
		if n == 0 {
			return "", io.ErrNoProgress
		}
	}
	return "", errors.New("protocol record exceeds limit")
}

func ensureAnchorAlive(owned *anchor) bool {
	if !anchorProcessAlive(owned.cmd.Process) {
		return false
	}
	select {
	case line, ok := <-owned.statusCh:
		if !ok {
			owned.statusCh = nil
			return false
		}
		// READY was consumed before target admission. Any later record means the
		// anchor reported an internal error or violated its private protocol.
		return strings.HasPrefix(line, "READY ") && anchorProcessAlive(owned.cmd.Process)
	case <-time.After(20 * time.Millisecond):
		return anchorProcessAlive(owned.cmd.Process)
	}
}

func takeSignal(signals <-chan os.Signal) os.Signal {
	select {
	case received := <-signals:
		return received
	default:
		return nil
	}
}

func signalName(received os.Signal) string {
	if received == os.Interrupt || received == syscall.SIGINT {
		return "INT"
	}
	return "TERM"
}

func signalExitCode(received os.Signal) int {
	if signalName(received) == "INT" {
		return 130
	}
	return 143
}

func commandExitCode(err error) (int, error) {
	if errors.Is(err, exec.ErrWaitDelay) {
		// The direct target exited successfully; WaitDelay only closed I/O
		// pipes retained by descendants so process-group cleanup could proceed.
		return 0, nil
	}
	return processExitCode(err)
}

func retireAnchor(owned *anchor, signal string, grace time.Duration, targetWait <-chan error) error {
	var controlErr error
	if owned.control != nil {
		_, writeErr := fmt.Fprintf(owned.control, "STOP %s\n", signal)
		closeErr := owned.control.Close()
		owned.control = nil
		if writeErr != nil && !errors.Is(writeErr, os.ErrClosed) {
			controlErr = fmt.Errorf("send anchor cleanup request: %w", writeErr)
		}
		if closeErr != nil {
			controlErr = errors.Join(controlErr, fmt.Errorf("close anchor control pipe: %w", closeErr))
		}
	}

	if owned.waitCh == nil {
		waitCh := make(chan error, 1)
		go func() {
			waitCh <- owned.cmd.Wait()
			close(waitCh)
		}()
		owned.waitCh = waitCh
	}
	deadline := time.NewTimer(grace + cleanupWaitAllowance)
	defer deadline.Stop()
	var anchorWaitErr error
	var cleanupFailure error
	anchorDone := false
	targetDone := targetWait == nil
	statusDone := owned.statusCh == nil
	for !anchorDone || !targetDone || !statusDone {
		select {
		case anchorWaitErr = <-owned.waitCh:
			anchorDone = true
			owned.waitCh = nil
		case <-targetWait:
			targetDone = true
			targetWait = nil
		case statusLine, ok := <-owned.statusCh:
			if !ok {
				owned.statusCh = nil
				statusDone = true
				continue
			}
			if strings.HasPrefix(statusLine, "FAIL ") {
				cleanupFailure = errors.Join(cleanupFailure, errors.New(strings.TrimPrefix(statusLine, "FAIL ")))
			} else if strings.HasPrefix(statusLine, "!ERROR ") {
				cleanupFailure = errors.Join(cleanupFailure, errors.New(strings.TrimPrefix(statusLine, "!ERROR ")))
			}
		case <-deadline.C:
			closeErr := owned.ready.Close()
			owned.ready = nil
			return errors.Join(fmt.Errorf("timed out retiring anchor and supervised command (grace %s)", grace), cleanupFailure, closeErr)
		}
	}
	if owned.ready != nil {
		_ = owned.ready.Close()
		owned.ready = nil
	}
	if anchorWaitErr == nil {
		anchorWaitErr = errors.New("anchor exited without the final process-group SIGKILL")
	} else if !expectedFinalGroupKill(anchorWaitErr) {
		anchorWaitErr = fmt.Errorf("anchor exited unexpectedly: %w", anchorWaitErr)
	} else {
		anchorWaitErr = nil
	}
	return errors.Join(controlErr, cleanupFailure, anchorWaitErr)
}

func (owned *anchor) closeFiles() {
	if owned.control != nil {
		_ = owned.control.Close()
		owned.control = nil
	}
	if owned.ready != nil {
		_ = owned.ready.Close()
		owned.ready = nil
	}
}
