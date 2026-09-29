//go:build darwin || linux

package tailscalecli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunCommandBoundsOutput(t *testing.T) {
	result, err := runCommand(context.Background(), "/bin/sh", "-c", "printf '%1200000s' x")
	if !errors.Is(err, ErrOutputTooLong) {
		t.Fatalf("oversized output error = %v, want ErrOutputTooLong", err)
	}
	if len(result.stdout) > MaxOutputBytes {
		t.Fatalf("retained %d output bytes, safety bound is %d", len(result.stdout), MaxOutputBytes)
	}
}

func TestRunCommandBoundsInheritedPipesAndKillsChildGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	script := `sleep 30 & echo $! > "$1"; exit 0`
	started := time.Now()
	_, err := runCommand(context.Background(), "/bin/sh", "-c", script, "cli-fixture", pidFile)
	if !errors.Is(err, ErrUncertain) {
		t.Fatalf("inherited pipe error = %v, want ErrUncertain", err)
	}
	if elapsed := time.Since(started); elapsed > ChildWaitDelay+time.Second {
		t.Fatalf("command wait exceeded bound: %v", elapsed)
	}

	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read child pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil || pid <= 0 {
		t.Fatalf("invalid child pid %q: %v", pidBytes, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if !processRunning(pid) || processIsZombie(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("child process %d survived bounded command cleanup", pid)
}

func processRunning(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func processIsZombie(pid int) bool {
	command := exec.Command("ps", "-o", "stat=", "-p", fmt.Sprint(pid))
	state, err := command.Output()
	return err == nil && strings.HasPrefix(strings.TrimSpace(string(state)), "Z")
}
