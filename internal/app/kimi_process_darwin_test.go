package app

import (
	"os/exec"
	"testing"
)

func TestKimiE2dDarwinOwnedProcessStart(t *testing.T) {
	start := func() string {
		t.Helper()
		child := exec.Command("/bin/sleep", "30")
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
		first, err := kimiProcessStart(child.Process.Pid)
		if err != nil || first == "" {
			t.Fatalf("owned process start unavailable: %v", err)
		}
		again, err := kimiProcessStart(child.Process.Pid)
		if err != nil || again != first {
			t.Fatal("owned process start unstable")
		}
		return first
	}
	if start() == start() {
		t.Fatal("separate owned processes share incarnation")
	}
}
