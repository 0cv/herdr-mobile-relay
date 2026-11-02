package processgroup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
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
