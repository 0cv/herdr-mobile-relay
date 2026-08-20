package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStrictPaneReadSchema(t *testing.T) {
	for _, source := range []string{"visible", "recent", "recent-unwrapped"} {
		valid := []string{"pane-1", "--lines", "80", "--source", source, "--format", "ansi"}
		if err := validatePaneRead(valid); err != nil {
			t.Fatalf("valid %s schema rejected: %v", source, err)
		}
	}
	for _, invalid := range [][]string{
		{"pane-1", "--format", "ansi", "--lines", "80", "--source", "recent-unwrapped"},
		{"pane-1", "--lines", "80", "--source", "recent-unwrapped", "--format", "html"},
		{"pane-1", "--lines", "80", "--source", "recent-unwrapped"},
		{"pane-1", "--lines", "80", "--source", "detection", "--format", "ansi"},
	} {
		if err := validatePaneRead(invalid); err == nil {
			t.Fatalf("invalid schema accepted: %q", invalid)
		}
	}
}

func TestOperationRecordIsCreatedAtStartAndCompletedInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "operations.jsonl")
	t.Setenv("FAKE_HERDR_OPERATIONS", path)
	started := time.Now().UTC()
	recordOperation(9, []string{"pane", "read"}, started, time.Time{}, "started", "delay")
	recordOperation(9, []string{"pane", "read"}, started, started.Add(time.Second), "succeeded", "delay")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var operations []Operation
	for _, line := range splitNonemptyLines(string(data)) {
		var operation Operation
		if err := json.Unmarshal([]byte(line), &operation); err != nil {
			t.Fatal(err)
		}
		operations = append(operations, operation)
	}
	if len(operations) != 1 || operations[0].Outcome != "succeeded" || operations[0].CompletedAt == "" {
		t.Fatalf("operations = %+v", operations)
	}
}

func splitNonemptyLines(value string) []string {
	var result []string
	start := 0
	for index := 0; index <= len(value); index++ {
		if index < len(value) && value[index] != '\n' {
			continue
		}
		if line := value[start:index]; line != "" {
			result = append(result, line)
		}
		start = index + 1
	}
	return result
}

func TestBarrierSignalsThenWaitsForRelease(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FAKE_HERDR_BARRIER_DIR", root)
	done := make(chan struct{})
	go func() {
		waitBarrier("dispatch", 7)
		close(done)
	}()
	arrived := filepath.Join(root, "dispatch.7.arrived")
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(arrived); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("barrier did not signal arrival")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-done:
		t.Fatal("barrier returned before release")
	default:
	}
	if err := os.WriteFile(filepath.Join(root, "dispatch.release"), []byte("released\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("barrier did not observe release")
	}
}
