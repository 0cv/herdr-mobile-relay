package app

import (
	"fmt"
	"os/exec"
	"testing"
)

func TestDarwinOwnedProcessStart(t *testing.T) {
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
		birth, err := processStartTime(t.Context(), child.Process.Pid)
		if err != nil || first != fmt.Sprintf("%d:%06d", birth.Unix(), birth.Nanosecond()/1000) {
			t.Fatalf("Kimi start differs from shared process start: %v", err)
		}
		registryStart, err := registryProcessStart(t.Context(), child.Process.Pid)
		if err != nil || registryStart != float64(birth.Unix())+float64(birth.Nanosecond())/1e9 {
			t.Fatalf("registry start differs from shared process start: %v", err)
		}
		return first
	}
	if start() == start() {
		t.Fatal("separate owned processes share incarnation")
	}
}
