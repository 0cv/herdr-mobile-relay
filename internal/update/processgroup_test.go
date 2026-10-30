package update

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunCommandContextPreCanceledDoesNotStartOrModifyCommand(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	command := exec.Command("/bin/sh", "-c", `printf started > "$1"`, "fixture", marker)
	originalAttr := &syscall.SysProcAttr{}
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	command.SysProcAttr = originalAttr
	command.WaitDelay = time.Second
	command.Stdout, command.Stderr = stdout, stderr
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	output, err := runCommandContext(ctx, command)
	if output != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled command = %q, %v", output, err)
	}
	if command.Process != nil || command.SysProcAttr != originalAttr || command.WaitDelay != time.Second ||
		command.Stdout != stdout || command.Stderr != stderr {
		t.Fatal("pre-canceled command was started or modified")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("command marker exists or cannot be checked: %v", err)
	}
}

func TestRunCommandContextStartFailureReturnsNilOutput(t *testing.T) {
	command := exec.Command(filepath.Join(t.TempDir(), "missing"))
	output, err := runCommandContext(context.Background(), command)
	if output != nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("start failure = %q, %v", output, err)
	}
}

func TestRunCommandContextOrdersStreamsOnExit(t *testing.T) {
	for _, code := range []int{0, 7} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			script := `printf 'err-1|' >&2; printf 'out-1|'; printf 'err-2|' >&2; printf 'out-2|'; exit "$1"`
			command := exec.Command("/bin/sh", "-c", script, "fixture", strconv.Itoa(code))
			originalAttr := &syscall.SysProcAttr{}
			command.SysProcAttr = originalAttr
			output, err := runCommandContext(context.Background(), command)
			if string(output) != "out-1|out-2|err-1|err-2|" {
				t.Fatalf("output = %q", output)
			}
			if code == 0 && err != nil {
				t.Fatalf("successful exit = %v", err)
			}
			if code != 0 {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != code {
					t.Fatalf("nonzero exit = %v", err)
				}
			}
			if command.SysProcAttr == originalAttr || !command.SysProcAttr.Setpgid || command.WaitDelay != 4*time.Second {
				t.Fatal("command did not receive a fresh process group and WaitDelay")
			}
		})
	}
}

func TestRunCommandContextCancellationOutputPolicy(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	// The marker is written only after the handler and both stream writes.
	// A shell-builtin loop avoids an extra child and exits as soon as TERM arrives.
	script := `trap 'printf "out-term|"; printf "err-term|" >&2; exit 0' TERM
printf 'err-start|' >&2
printf 'out-start|'
printf 'ready:%s:end\n' "$$" > "$1"
while :; do :; done`
	command := exec.Command("/bin/sh", "-c", script, "fixture", ready)
	ctx, cancel := context.WithCancel(context.Background())
	var output []byte
	var commandErr error
	done := make(chan struct{})
	go func() {
		output, commandErr = runCommandContext(ctx, command)
		close(done)
	}()

	pid := 0
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
			return
		case <-time.After(12 * time.Second):
			if pid > 1 {
				_ = syscall.Kill(-pid, syscall.SIGKILL)
			}
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("fixture did not stop after cleanup")
		}
	})

	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for pid == 0 {
		data, err := os.ReadFile(ready)
		if err == nil {
			value := strings.TrimSpace(string(data))
			if strings.HasPrefix(value, "ready:") && strings.HasSuffix(value, ":end") {
				pid, err = strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(value, "ready:"), ":end"))
				if err != nil || pid <= 1 {
					t.Fatalf("invalid fixture pid %q: %v", value, err)
				}
				break
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read readiness marker: %v", err)
		}
		select {
		case <-done:
			t.Fatalf("fixture exited before readiness: %v", commandErr)
		case <-deadline.C:
			t.Fatal("fixture did not become ready")
		case <-ticker.C:
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(12 * time.Second):
		t.Fatal("cancellation did not complete within its bound")
	}
	if !errors.Is(commandErr, context.Canceled) {
		t.Fatalf("cancellation error = %v", commandErr)
	}
	if string(output) != "out-start|out-term|err-start|err-term|" {
		t.Fatalf("cancellation output = %q", output)
	}
}
