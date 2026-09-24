//go:build linux || darwin

package processsupervisor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const helperMarker = "process-supervisor-helper"

type processWait struct {
	done chan struct{}
	err  error
}

var processWaits sync.Map

func TestProcessSupervisorHelper(t *testing.T) {
	args := helperArguments(os.Args)
	if len(args) == 0 {
		return
	}
	mode, args := args[0], args[1:]
	switch mode {
	case "anchor":
		code, err := RunAnchor()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(code)
	case "anchor-early":
		os.Exit(82)
	case "anchor-dead-ready":
		control := os.NewFile(3, "test-control")
		ready := os.NewFile(4, "test-readiness")
		start, err := readRecord(control, maxProtocolRecord)
		if err != nil {
			os.Exit(81)
		}
		fields := strings.Fields(start)
		pid := os.Getpid()
		pgid, _ := processGroupID(pid)
		_, _ = fmt.Fprintf(ready, "READY %s %d %d %d\n", fields[1], pid, os.Getppid(), pgid)
		os.Exit(80)
	case "anchor-mismatch":
		control := os.NewFile(3, "test-control")
		ready := os.NewFile(4, "test-readiness")
		start, err := readRecord(control, maxProtocolRecord)
		if err != nil {
			os.Exit(83)
		}
		fields := strings.Fields(start)
		if len(fields) != 3 {
			os.Exit(84)
		}
		pid := os.Getpid()
		pgid, _ := processGroupID(pid)
		_, _ = fmt.Fprintf(ready, "READY %s %d %d %d\n", fields[1], pid+1, os.Getppid(), pgid)
		os.Exit(85)
	case "supervisor":
		if len(args) < 2 {
			os.Exit(86)
		}
		grace, err := time.ParseDuration(args[0])
		if err != nil {
			os.Exit(87)
		}
		code, err := run(args[1:], grace, Streams{
			Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
		}, runConfig{
			selfPath:   os.Args[0],
			anchorArgs: helperCommand("anchor")[1:],
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(code)
	case "target":
		if len(args) == 0 {
			os.Exit(88)
		}
		switch args[0] {
		case "exit":
			code, _ := strconv.Atoi(args[1])
			os.Exit(code)
		case "mark":
			if len(args) != 2 {
				os.Exit(97)
			}
			_ = os.WriteFile(args[1], []byte("started"), 0600)
			os.Exit(0)
		case "stdio":
			input, _ := io.ReadAll(os.Stdin)
			fmt.Fprintf(os.Stderr, "stderr:%s\n", os.Getenv("HERDR_PROCESS_TEST_ENV"))
			_ = json.NewEncoder(os.Stdout).Encode(struct {
				Args  []string `json:"args"`
				Input string   `json:"input"`
				Env   string   `json:"env"`
			}{Args: args[1:], Input: string(input), Env: os.Getenv("HERDR_PROCESS_TEST_ENV")})
			os.Exit(0)
		case "spawn-exit", "spawn-stay":
			spawnTestDescendant(args[1:])
		case "descendant":
			descendant(args[1:])
		case "foreign":
			if len(args) != 2 {
				os.Exit(89)
			}
			_ = os.WriteFile(args[1], []byte(strconv.Itoa(os.Getpid())), 0600)
			for {
				time.Sleep(time.Hour)
			}
		default:
			os.Exit(90)
		}
	default:
		os.Exit(91)
	}
}

func helperArguments(args []string) []string {
	for index, arg := range args {
		if arg == "--" && index+1 < len(args) && args[index+1] == helperMarker {
			return args[index+2:]
		}
	}
	return nil
}

func helperCommand(args ...string) []string {
	command := []string{os.Args[0], "-test.run=^TestProcessSupervisorHelper$", "--", helperMarker}
	return append(command, args...)
}

func spawnTestDescendant(args []string) {
	if len(args) < 3 {
		os.Exit(92)
	}
	pidFile, readyFile, targetMode := args[0], args[1], args[2]
	childArgs := []string{"target", "descendant", readyFile, targetMode}
	if targetMode == "ack-int" {
		if len(args) < 4 {
			os.Exit(96)
		}
		childArgs = append(childArgs, args[3])
	}
	child := helperExec(helperCommand(childArgs...))
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		os.Exit(93)
	}
	_ = os.WriteFile(pidFile, []byte(strconv.Itoa(child.Process.Pid)), 0600)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(readyFile); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := os.Stat(readyFile); err != nil {
		_ = child.Process.Kill()
		os.Exit(94)
	}
	if len(args) > 3 && args[3] == "exit" {
		if len(args) < 5 {
			os.Exit(96)
		}
		code, _ := strconv.Atoi(args[4])
		os.Exit(code)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func signalIgnore() {
	signal.Ignore(os.Interrupt, syscall.SIGTERM)
}

func descendant(args []string) {
	if len(args) < 2 || len(args) > 3 {
		os.Exit(95)
	}
	readyFile, mode := args[0], args[1]
	var interrupt <-chan os.Signal
	switch mode {
	case "stubborn":
		if len(args) != 2 {
			os.Exit(95)
		}
		signalIgnore()
	case "ack-int":
		if len(args) != 3 {
			os.Exit(95)
		}
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, os.Interrupt)
		defer signal.Stop(signals)
		interrupt = signals
	default:
		if len(args) != 2 {
			os.Exit(95)
		}
	}
	_ = os.WriteFile(readyFile, []byte(strconv.Itoa(os.Getpid())), 0600)
	if interrupt != nil {
		<-interrupt
		_ = os.WriteFile(args[2], []byte("INT"), 0600)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func TestRunPropagatesExitOnlyAfterGroupRetirement(t *testing.T) {
	for _, code := range []int{0, 23} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			foreign := startForeignFixture(t)
			directory := t.TempDir()
			pidFile := filepath.Join(directory, "descendant.pid")
			readyFile := filepath.Join(directory, "descendant.ready")
			cmd := startSupervisor(t, 180*time.Millisecond, helperCommand("target", "spawn-exit", pidFile, readyFile, "stubborn", "exit", strconv.Itoa(code))...)
			pid := waitPIDFile(t, pidFile)
			started := time.Now()
			err := waitCommand(t, cmd, 4*time.Second)
			if got := exitStatus(err); got != code {
				t.Fatalf("supervisor exit = %d (err %v), want %d", got, err, code)
			}
			if elapsed := time.Since(started); elapsed < 140*time.Millisecond || elapsed > 4*time.Second {
				t.Fatalf("cleanup duration %s outside expected bound", elapsed)
			}
			assertProcessRetired(t, pid)
			assertForeignSurvives(t, foreign)
		})
	}
}

func TestNonFileOutputDescendantCannotBlockGroupCleanup(t *testing.T) {
	foreign := startForeignFixture(t)
	for _, exitCode := range []int{0, 23} {
		t.Run(strconv.Itoa(exitCode), func(t *testing.T) {
			directory := t.TempDir()
			pidFile := filepath.Join(directory, "descendant.pid")
			readyFile := filepath.Join(directory, "descendant.ready")
			command := helperCommand("target", "spawn-exit", pidFile, readyFile, "stubborn", "exit", strconv.Itoa(exitCode))
			var stdout, stderr bytes.Buffer
			type runResult struct {
				code int
				err  error
			}
			completed := make(chan runResult, 1)
			grace := 150 * time.Millisecond
			started := time.Now()
			go func() {
				code, err := run(command, grace, Streams{Stdout: &stdout, Stderr: &stderr}, runConfig{
					selfPath:   os.Args[0],
					anchorArgs: helperCommand("anchor")[1:],
				})
				completed <- runResult{code: code, err: err}
			}()

			pid := waitPIDFile(t, pidFile)
			timeout := targetStreamWaitDelay + grace + cleanupWaitAllowance + 2*time.Second
			select {
			case result := <-completed:
				if result.err != nil || result.code != exitCode {
					t.Fatalf("run() = (%d, %v), want direct target exit %d", result.code, result.err, exitCode)
				}
			case <-time.After(timeout):
				t.Fatalf("group cleanup did not finish within %s with descendant-held non-file output pipes", timeout)
			}
			if elapsed := time.Since(started); elapsed < targetStreamWaitDelay/2 || elapsed > timeout {
				t.Fatalf("run duration %s did not exercise the bounded stream wait or exceeded %s", elapsed, timeout)
			}
			assertProcessRetired(t, pid)
			assertForeignSurvives(t, foreign)
		})
	}
}

func TestRunPreservesEnvironmentStdinStreamsAndArgumentBoundaries(t *testing.T) {
	foreign := startForeignFixture(t)
	previous, hadPrevious := os.LookupEnv("HERDR_PROCESS_TEST_ENV")
	if err := os.Setenv("HERDR_PROCESS_TEST_ENV", "inherited value"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if hadPrevious {
			_ = os.Setenv("HERDR_PROCESS_TEST_ENV", previous)
		} else {
			_ = os.Unsetenv("HERDR_PROCESS_TEST_ENV")
		}
	})

	arguments := []string{"with space", "'quoted'", "$(not-a-shell)", ";touch /tmp/not-created"}
	command := helperCommand(append([]string{"target", "stdio"}, arguments...)...)
	var stdout, stderr bytes.Buffer
	code, err := run(command, time.Second, Streams{
		Stdin: strings.NewReader("stdin payload\n"), Stdout: &stdout, Stderr: &stderr,
	}, runConfig{selfPath: os.Args[0], anchorArgs: helperCommand("anchor")[1:]})
	if err != nil || code != 0 {
		t.Fatalf("run() = (%d, %v)", code, err)
	}
	var received struct {
		Args  []string `json:"args"`
		Input string   `json:"input"`
		Env   string   `json:"env"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &received); err != nil {
		t.Fatalf("target stdout %q is not JSON: %v", stdout.String(), err)
	}
	if !equalStrings(received.Args, arguments) {
		t.Fatalf("target args = %#v, want %#v", received.Args, arguments)
	}
	if received.Input != "stdin payload\n" || received.Env != "inherited value" {
		t.Fatalf("target input/env = (%q, %q)", received.Input, received.Env)
	}
	if !strings.Contains(stderr.String(), "stderr:inherited value") {
		t.Fatalf("target stderr = %q", stderr.String())
	}
	assertForeignSurvives(t, foreign)
}

func TestSupervisorSignalsMapAndRetireGroup(t *testing.T) {
	for _, test := range []struct {
		signal syscall.Signal
		want   int
	}{{syscall.SIGINT, 130}, {syscall.SIGTERM, 143}} {
		t.Run(test.signal.String(), func(t *testing.T) {
			foreign := startForeignFixture(t)
			directory := t.TempDir()
			pidFile := filepath.Join(directory, "descendant.pid")
			readyFile := filepath.Join(directory, "descendant.ready")
			cmd := startSupervisor(t, 150*time.Millisecond, helperCommand("target", "spawn-stay", pidFile, readyFile, "stubborn")...)
			pid := waitPIDFile(t, pidFile)
			waitForFile(t, readyFile)
			if err := cmd.Process.Signal(test.signal); err != nil {
				t.Fatalf("signal supervisor: %v", err)
			}
			started := time.Now()
			if got := exitStatus(waitCommand(t, cmd, 4*time.Second)); got != test.want {
				t.Fatalf("supervisor exit = %d, want %d", got, test.want)
			}
			if elapsed := time.Since(started); elapsed > 4*time.Second {
				t.Fatalf("cleanup exceeded bound: %s", elapsed)
			}
			assertProcessRetired(t, pid)
			assertForeignSurvives(t, foreign)
		})
	}
}

func TestDescendantHoldingOutputDescriptorsCannotBlockSupervisor(t *testing.T) {
	foreign := startForeignFixture(t)
	directory := t.TempDir()
	pidFile := filepath.Join(directory, "descendant.pid")
	readyFile := filepath.Join(directory, "descendant.ready")
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	targetCommand := helperCommand("target", "spawn-exit", pidFile, readyFile, "stubborn", "exit", "0")
	supervisorArgs := append([]string{"supervisor", "150ms"}, targetCommand...)
	cmd := helperExec(helperCommand(supervisorArgs...))
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devNull, writeEnd, devNull
	if err := cmd.Start(); err != nil {
		_ = devNull.Close()
		_ = readEnd.Close()
		_ = writeEnd.Close()
		t.Fatal(err)
	}
	_ = devNull.Close()
	_ = writeEnd.Close()
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = waitCommand(t, cmd, 3*time.Second)
		}
	})
	pid := waitPIDFile(t, pidFile)
	waitStarted := time.Now()
	if got := exitStatus(waitCommand(t, cmd, 4*time.Second)); got != 0 {
		_ = readEnd.Close()
		t.Fatalf("supervisor exit = %d, want 0", got)
	}
	if elapsed := time.Since(waitStarted); elapsed > 4*time.Second {
		_ = readEnd.Close()
		t.Fatalf("supervisor blocked on inherited output for %s", elapsed)
	}
	readResult := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, readEnd)
		readResult <- err
	}()
	select {
	case err := <-readResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		_ = readEnd.Close()
		t.Fatal("descendant kept inherited stdout open after supervisor exit")
	}
	_ = readEnd.Close()
	assertProcessRetired(t, pid)
	assertForeignSurvives(t, foreign)
}

func TestSupervisorKillClosesControlAndAnchorRetiresGroup(t *testing.T) {
	foreign := startForeignFixture(t)
	directory := t.TempDir()
	pidFile := filepath.Join(directory, "descendant.pid")
	readyFile := filepath.Join(directory, "descendant.ready")
	cmd := startSupervisor(t, 150*time.Millisecond, helperCommand("target", "spawn-stay", pidFile, readyFile, "stubborn")...)
	pid := waitPIDFile(t, pidFile)
	waitForFile(t, readyFile)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill supervisor helper: %v", err)
	}
	_ = waitCommand(t, cmd, 3*time.Second)
	assertProcessRetired(t, pid)
	assertForeignSurvives(t, foreign)
}

func TestForeignProcessGroupSurvivesCleanup(t *testing.T) {
	directory := t.TempDir()
	foreign := startForeignFixture(t)

	pidFile := filepath.Join(directory, "descendant.pid")
	readyFile := filepath.Join(directory, "descendant.ready")
	target := startSupervisor(t, 140*time.Millisecond, helperCommand("target", "spawn-exit", pidFile, readyFile, "stubborn", "exit", "0")...)
	descendantPID := waitPIDFile(t, pidFile)
	if got := exitStatus(waitCommand(t, target, 4*time.Second)); got != 0 {
		t.Fatalf("supervisor exit = %d, want 0", got)
	}
	assertProcessRetired(t, descendantPID)
	assertForeignSurvives(t, foreign)
}

func startForeignFixture(t *testing.T) *exec.Cmd {
	t.Helper()
	foreignReady := filepath.Join(t.TempDir(), "foreign.ready")
	foreign := helperExec(helperCommand("target", "foreign", foreignReady))
	foreign.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	foreign.Stdin, foreign.Stdout, foreign.Stderr = devNull, devNull, devNull
	if err := foreign.Start(); err != nil {
		_ = devNull.Close()
		t.Fatal(err)
	}
	_ = devNull.Close()
	t.Cleanup(func() {
		_ = foreign.Process.Kill()
		_ = foreign.Wait()
	})
	waitForFile(t, foreignReady)
	return foreign
}

func assertForeignSurvives(t *testing.T, foreign *exec.Cmd) {
	t.Helper()
	if foreign.ProcessState != nil {
		t.Fatal("foreign fixture exited during process-group supervision")
	}
	if err := foreign.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("foreign fixture did not survive: %v", err)
	}
}

func TestAnchorReadinessAndTargetAssignmentFailuresFailClosed(t *testing.T) {
	foreign := startForeignFixture(t)
	for _, test := range []struct {
		name       string
		anchorArgs []string
	}{
		{name: "readiness generation mismatch", anchorArgs: helperCommand("anchor-mismatch")[1:]},
		{name: "early anchor exit", anchorArgs: helperCommand("anchor-early")[1:]},
		{name: "anchor exited after readiness", anchorArgs: helperCommand("anchor-dead-ready")[1:]},
	} {
		t.Run(test.name, func(t *testing.T) {
			targetMarker := filepath.Join(t.TempDir(), "target-started")
			_, err := run(helperCommand("target", "mark", targetMarker), 100*time.Millisecond, Streams{}, runConfig{
				selfPath: os.Args[0], anchorArgs: test.anchorArgs,
			})
			if err == nil {
				t.Fatal("invalid anchor accepted")
			}
			if _, err := os.Stat(targetMarker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("target ran despite anchor refusal (stat error %v)", err)
			}
			assertForeignSurvives(t, foreign)
		})
	}
	targetMarker := filepath.Join(t.TempDir(), "target-started")
	if _, err := startTarget(helperCommand("target", "mark", targetMarker), -1, Streams{}); err == nil {
		t.Fatal("invalid PGID assignment unexpectedly started target")
	}
	if _, err := os.Stat(targetMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target ran despite group-assignment failure (stat error %v)", err)
	}
	assertForeignSurvives(t, foreign)
}

func TestCompetingSignalsKeepTheFirstExitMapping(t *testing.T) {
	foreign := startForeignFixture(t)
	directory := t.TempDir()
	pidFile := filepath.Join(directory, "descendant.pid")
	readyFile := filepath.Join(directory, "descendant.ready")
	interruptFile := filepath.Join(directory, "descendant.interrupt")
	cmd := startSupervisor(t, 2*time.Second, helperCommand("target", "spawn-stay", pidFile, readyFile, "ack-int", interruptFile)...)
	pid := waitPIDFile(t, pidFile)
	waitForFile(t, readyFile)
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, interruptFile)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("send competing TERM during INT cleanup: %v", err)
	}
	if got := exitStatus(waitCommand(t, cmd, 5*time.Second)); got != 130 {
		t.Fatalf("competing signals returned %d, want first-signal status 130", got)
	}
	assertProcessRetired(t, pid)
	assertForeignSurvives(t, foreign)
}

func TestPrivateAnchorRefusesMissingDescriptors(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessSupervisorHelper$", "--", helperMarker, "anchor")
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Run(); err == nil {
		t.Fatal("anchor succeeded without inherited private descriptors")
	}
}

func helperExec(args []string) *exec.Cmd {
	if len(args) == 0 {
		panic("empty helper command")
	}
	return exec.Command(args[0], args[1:]...)
}

func startSupervisor(t *testing.T, grace time.Duration, target ...string) *exec.Cmd {
	t.Helper()
	graceText := grace.String()
	args := append([]string{"supervisor", graceText}, target...)
	cmd := helperExec(helperCommand(args...))
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devNull, devNull, os.Stderr
	if err := cmd.Start(); err != nil {
		_ = devNull.Close()
		t.Fatal(err)
	}
	_ = devNull.Close()
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = waitCommand(t, cmd, 3*time.Second)
		}
	})
	return cmd
}

func waitCommand(t *testing.T, cmd *exec.Cmd, timeout time.Duration) error {
	t.Helper()
	result := &processWait{done: make(chan struct{})}
	actual, loaded := processWaits.LoadOrStore(cmd, result)
	if loaded {
		result = actual.(*processWait)
	} else {
		go func() {
			result.err = cmd.Wait()
			close(result.done)
		}()
	}
	select {
	case <-result.done:
		return result.err
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		t.Fatalf("process %d did not exit within %s", cmd.Process.Pid, timeout)
		return errors.New("unreachable")
	}
}

func waitPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var data []byte
	var parseErr error
	for time.Now().Before(deadline) {
		var err error
		data, err = os.ReadFile(path)
		if err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err == nil && pid > 0 {
				return pid
			}
			if err != nil {
				parseErr = err
			} else {
				parseErr = errors.New("PID must be positive")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if parseErr == nil {
		parseErr = errors.New("PID file was not created")
	}
	t.Fatalf("invalid fixture PID in %s: %q (%v)", path, data, parseErr)
	return 0
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("fixture did not create %s", path)
}

func assertProcessRetired(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if processRetired(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("owned descendant pid %d survived supervisor cleanup", pid)
}

func exitStatus(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestWaitBoundsIncludeConfiguredGrace(t *testing.T) {
	if maxGrace+cleanupWaitAllowance <= maxGrace {
		t.Fatal("cleanup allowance must leave scheduling room after configured grace")
	}
}
