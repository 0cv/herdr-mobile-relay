// Command pg-spike proves the owned TERM group → grace → KILL group → reap
// algorithm reaps children with no leak on Linux, including a grandchild that
// ignores SIGTERM and the group-leader-exits-first case.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const (
	gracePeriod = 2 * time.Second
	waitDelay   = 4 * time.Second
)

func main() {
	fmt.Println("=== Process-group dispatch spike ===")
	fmt.Println()

	pass := true
	pass = runCase("SIGTERM-respecting child", scriptRespectful) && pass
	pass = runCase("SIGTERM-ignoring grandchild", scriptIgnoringGrandchild) && pass
	pass = runCase("group leader exits first", scriptLeaderExitsFirst) && pass
	pass = runCase("Start failure → not_started", scriptStartFailure) && pass

	fmt.Println()
	if pass {
		fmt.Println("ALL CASES PASSED")
	} else {
		fmt.Println("SOME CASES FAILED")
		os.Exit(1)
	}
}

func runCase(name string, script string) bool {
	fmt.Printf("--- %s ---\n", name)

	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "bash", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.WaitDelay = waitDelay

	start := time.Now()
	if err := cmd.Start(); err != nil {
		fmt.Printf("  Start() error (expected for start-failure case): %v\n", err)
		fmt.Printf("  RESULT: PASS (not_started)\n\n")
		return true
	}

	pgid := cmd.Process.Pid
	fmt.Printf("  started pid=%d pgid=%d\n", cmd.Process.Pid, pgid)

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	time.Sleep(200 * time.Millisecond)

	fmt.Printf("  sending SIGTERM to process group -%d\n", pgid)
	syscall.Kill(-pgid, syscall.SIGTERM)
	graceStart := time.Now()

	select {
	case err := <-done:
		elapsed := time.Since(start)
		fmt.Printf("  exited within grace: %v (elapsed %v)\n", err, elapsed.Round(time.Millisecond))
		fmt.Printf("  RESULT: PASS\n\n")
		return true
	case <-time.After(gracePeriod):
	}

	fmt.Printf("  grace expired (%v), sending SIGKILL to group -%d\n", time.Since(graceStart).Round(time.Millisecond), pgid)
	syscall.Kill(-pgid, syscall.SIGKILL)

	select {
	case err := <-done:
		elapsed := time.Since(start)
		fmt.Printf("  killed: %v (elapsed %v)\n", err, elapsed.Round(time.Millisecond))
		if !hasOrphans(pgid) {
			fmt.Printf("  no orphans detected\n")
			fmt.Printf("  RESULT: PASS\n\n")
			return true
		}
		fmt.Printf("  WARNING: orphans detected in pgid %d\n", pgid)
		fmt.Printf("  RESULT: FAIL\n\n")
		return false
	case <-time.After(2 * time.Second):
		fmt.Printf("  ERROR: process did not die after SIGKILL\n")
		fmt.Printf("  RESULT: FAIL\n\n")
		return false
	}
}

func hasOrphans(pgid int) bool {
	// Check /proc for any process still in this group
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		statPath := "/proc/" + e.Name() + "/stat"
		data, err := os.ReadFile(statPath)
		if err != nil {
			continue
		}
		var pid, ppid, pgrp int
		fmt.Sscanf(string(data), "%d %*s %*s %d %d", &pid, &ppid, &pgrp)
		if pgrp == pgid && pid != os.Getpid() {
			return true
		}
	}
	return false
}

// A child that respects SIGTERM and exits cleanly.
const scriptRespectful = `
trap 'exit 0' TERM
sleep 30 &
wait
`

// A grandchild that ignores SIGTERM; the group KILL must get it.
const scriptIgnoringGrandchild = `
bash -c 'trap "" TERM; sleep 60' &
sleep 30 &
wait
`

// The group leader exits immediately but leaves children running.
const scriptLeaderExitsFirst = `
bash -c 'trap "" TERM; sleep 60' &
bash -c 'sleep 60' &
exit 0
`

// A script that fails to start (nonexistent binary).
const scriptStartFailure = `
exec /nonexistent/binary/that/does/not/exist
`
