package processgroup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// This fixture starts a writer in a separate session, so it retains the
// command's output pipes after the supervised group has been terminated.
func TestProcessGroupFixture(t *testing.T) {
	role := os.Getenv("PROCESSGROUP_TEST_ROLE")
	if role == "" {
		return
	}
	dir := os.Getenv("PROCESSGROUP_TEST_DIR")
	switch role {
	case "leader":
		writer := exec.Command(os.Args[0], "-test.run=^TestProcessGroupFixture$")
		writer.Env = append(os.Environ(), "PROCESSGROUP_TEST_ROLE=writer")
		writer.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		writer.Stdout, writer.Stderr = os.Stdout, os.Stderr
		if err := writer.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Second)
	case "resistant":
		signal.Ignore(syscall.SIGTERM)
		_, _ = fmt.Fprint(os.Stdout, "out|")
		_, _ = fmt.Fprint(os.Stderr, "err|")
		if err := os.WriteFile(filepath.Join(dir, "ready"), []byte("ready"), 0o600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Second)
	case "writer":
		_, _ = fmt.Fprint(os.Stdout, "ready-output|")
		if err := os.WriteFile(filepath.Join(dir, "ready"), []byte("ready"), 0o600); err != nil {
			t.Fatal(err)
		}
		defer os.WriteFile(filepath.Join(dir, "stopped"), []byte("stopped"), 0o600)
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(filepath.Join(dir, "stop")); err == nil {
				return
			}
			_, _ = fmt.Fprint(os.Stdout, "out|")
			_, _ = fmt.Fprint(os.Stderr, "err|")
			time.Sleep(10 * time.Millisecond)
		}
	default:
		t.Fatalf("unknown fixture role %q", role)
	}
}

func TestRunReturnsOutputAfterKillAndCompletedWait(t *testing.T) {
	dir := t.TempDir()
	command := exec.Command(os.Args[0], "-test.run=^TestProcessGroupFixture$")
	command.Env = append(os.Environ(), "PROCESSGROUP_TEST_ROLE=resistant", "PROCESSGROUP_TEST_DIR="+dir)
	ctx, cancel := context.WithCancel(context.Background())
	var output []byte
	var commandErr error
	done := make(chan struct{})
	go func() {
		output, commandErr = Run(ctx, command, Options{
			PostKillWait:         4 * time.Second,
			ReturnOutputOnCancel: true,
		})
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("TERM-resistant fixture did not stop")
		}
	})

	if !waitForFixtureFile(filepath.Join(dir, "ready"), 5*time.Second) {
		t.Fatal("TERM-resistant fixture did not become ready")
	}
	started := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	if elapsed := time.Since(started); elapsed < 1800*time.Millisecond {
		t.Fatalf("TERM-resistant fixture stopped before escalation: %v", elapsed)
	}
	if string(output) != "out|err|" || !errors.Is(commandErr, context.Canceled) {
		t.Fatalf("completed Wait after KILL = %q, %v", output, commandErr)
	}
}

func TestRunDoesNotReadOutputBeforeWaitCompletes(t *testing.T) {
	dir := t.TempDir()
	command := exec.Command(os.Args[0], "-test.run=^TestProcessGroupFixture$")
	command.Env = append(os.Environ(), "PROCESSGROUP_TEST_ROLE=leader", "PROCESSGROUP_TEST_DIR="+dir)
	ctx, cancel := context.WithCancel(context.Background())
	var output []byte
	var commandErr error
	done := make(chan struct{})
	go func() {
		output, commandErr = Run(ctx, command, Options{
			PostKillWait:         30 * time.Millisecond,
			ReturnOutputOnCancel: true,
		})
		close(done)
	}()

	ready := false
	t.Cleanup(func() {
		cancel()
		if err := os.WriteFile(filepath.Join(dir, "stop"), []byte("stop"), 0o600); err != nil {
			t.Error(err)
		}
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("supervised fixture did not stop")
		}
		if ready && !waitForFixtureFile(filepath.Join(dir, "stopped"), 5*time.Second) {
			t.Error("escaped fixture writer did not stop")
		}
	})

	if !waitForFixtureFile(filepath.Join(dir, "ready"), 5*time.Second) {
		t.Fatal("escaped fixture writer did not become ready")
	}
	ready = true
	started := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	if elapsed := time.Since(started); elapsed >= 3900*time.Millisecond {
		t.Fatalf("cancellation took %v; could have reached the 4s WaitDelay", elapsed)
	}
	if output != nil || !errors.Is(commandErr, context.Canceled) {
		t.Fatalf("incomplete Wait = %q, %v; want nil, context canceled", output, commandErr)
	}
}

func waitForFixtureFile(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func TestRunOptions(t *testing.T) {
	for _, options := range []Options{
		{PostKillWait: 4 * time.Second, ReturnOutputOnCancel: true},
		{PostKillWait: 2 * time.Second, ReturnOutputOnCancel: false},
	} {
		t.Run(fmt.Sprintf("return_output_%t", options.ReturnOutputOnCancel), func(t *testing.T) {
			t.Run("PreCanceledDoesNotStartOrModifyCommand", func(t *testing.T) {
				marker := filepath.Join(t.TempDir(), "started")
				command := exec.Command("/bin/sh", "-c", `printf started > "$1"`, "fixture", marker)
				originalAttr := &syscall.SysProcAttr{}
				stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
				command.SysProcAttr = originalAttr
				command.WaitDelay = time.Second
				command.Stdout, command.Stderr = stdout, stderr
				ctx, cancel := context.WithCancel(context.Background())
				cancel()

				output, err := Run(ctx, command, options)
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
			})

			t.Run("StartFailureReturnsNilOutput", func(t *testing.T) {
				command := exec.Command(filepath.Join(t.TempDir(), "missing"))
				output, err := Run(context.Background(), command, options)
				if output != nil || !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("start failure = %q, %v", output, err)
				}
			})

			t.Run("OrdersStreamsOnExit", func(t *testing.T) {
				for _, code := range []int{0, 7} {
					t.Run(strconv.Itoa(code), func(t *testing.T) {
						script := `printf 'err-1|' >&2; printf 'out-1|'; printf 'err-2|' >&2; printf 'out-2|'; exit "$1"`
						command := exec.Command("/bin/sh", "-c", script, "fixture", strconv.Itoa(code))
						originalAttr := &syscall.SysProcAttr{}
						command.SysProcAttr = originalAttr
						output, err := Run(context.Background(), command, options)
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
			})

			t.Run("CancellationOutputPolicy", func(t *testing.T) {
				ready := filepath.Join(t.TempDir(), "ready")
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
					output, commandErr = Run(ctx, command, options)
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
				if options.ReturnOutputOnCancel {
					if string(output) != "out-start|out-term|err-start|err-term|" {
						t.Fatalf("cancellation output = %q", output)
					}
				} else if output != nil {
					t.Fatalf("cancellation output = %q, want nil", output)
				}
			})
		})
	}
}

func TestRunTerminatesDescendantsHoldingOutputPipes(t *testing.T) {
	options := Options{PostKillWait: 4 * time.Second, ReturnOutputOnCancel: true}
	for name, scriptBody := range map[string]string{
		"leader_exits_on_term": "#!/bin/sh\n(trap '' TERM; sleep 30) &\nchild=$!\nprintf '%%s\\n' \"$child\" > %q\nwait\n",
		"leader_ignores_term":  "#!/bin/sh\ntrap '' TERM\nsleep 30 &\nchild=$!\nprintf '%%s\\n' \"$child\" > %q\nwait\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			pidPath := filepath.Join(root, "child.pid")
			scriptPath := filepath.Join(root, "spawn-child.sh")
			script := fmt.Sprintf(scriptBody, pidPath)
			if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			command := exec.Command("/bin/sh", scriptPath)
			var commandErr error
			done := make(chan struct{})
			go func() {
				_, commandErr = Run(ctx, command, options)
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Fatal("Run did not return after cancelling a descendant-held pipe")
			}
			if !errors.Is(commandErr, context.DeadlineExceeded) {
				t.Fatalf("Run() error = %v, want deadline exceeded", commandErr)
			}

			pidData, err := os.ReadFile(pidPath)
			if err != nil {
				t.Fatal(err)
			}
			childPID, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = syscall.Kill(childPID, syscall.SIGKILL)
			})
			deadline := time.Now().Add(time.Second)
			for {
				err := syscall.Kill(childPID, 0)
				if err != nil && !errors.Is(err, syscall.EPERM) {
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("descendant process %d survived process-group cancellation", childPID)
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}
