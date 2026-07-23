//go:build unix

package herdr

// Regression test for ADR-013: a cancelled Herdr invocation must terminate the
// whole process group, not just the leader.

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCancelledCommandDoesNotLeakGroup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	t.Setenv("LEAK_PIDFILE", pidFile)

	// Leader backgrounds a child (same process group) that records its PID,
	// ignores SIGTERM, and loops. Child stdout is detached so Wait can return
	// promptly once the leader dies.
	script := "#!/bin/sh\n" +
		"sh -c 'echo $$ > \"$LEAK_PIDFILE\"; trap \"\" TERM; while true; do sleep 1; done' >/dev/null 2>&1 &\n" +
		"wait\n"
	bin := filepath.Join(dir, "herdr")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	c := NewClient(bin, filepath.Join(dir, "sock"))

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond) // let the child write its PID
		cancel()                           // parent cancellation (not a deadline)
	}()

	// Runs the leader; returns once cancellation kills it.
	_, _ = c.run(ctx, 10*time.Second, "send-text", "--pane", "p1", "x")

	pid := readChildPID(t, pidFile)
	if pid == 0 {
		t.Fatal("child never recorded its PID")
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	if alive(pid) {
		t.Fatalf("grandchild pid %d survived ADR-013 TERM-to-KILL cancellation", pid)
	}
}

func readChildPID(t *testing.T, path string) int {
	t.Helper()
	for i := 0; i < 50; i++ {
		if b, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return 0
}

func alive(pid int) bool {
	// signal 0 probes existence without delivering a signal.
	return syscall.Kill(pid, 0) == nil
}
