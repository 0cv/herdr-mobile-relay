package main

import (
	"os"
	"testing"
)

func TestAgentSessionAlwaysSilentSuccess(t *testing.T) {
	t.Setenv("HC_TERMINAL_ID", "t1")
	t.Setenv("HC_TERMINAL_SOCKET", t.TempDir()+"/missing.sock")
	input, err := os.CreateTemp(t.TempDir(), "input")
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if _, err = input.WriteString(`{"session_id":"abc","hook_event_name":"SessionStart"}`); err != nil {
		t.Fatal(err)
	}
	output, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = input, output
	defer func() { os.Stdin, os.Stdout = oldIn, oldOut }()
	for _, args := range [][]string{{"agent-session"}, {"agent-session", "unknown"}, {"agent-session", "claude", "extra"}, {"agent-session", "claude"}, {"agent-session", "codex"}} {
		if _, err := input.Seek(0, 0); err != nil {
			t.Fatal(err)
		}
		code, err := run(args)
		if code != 0 || err != nil {
			t.Fatalf("%v: %d %v", args, code, err)
		}
	}
	stat, err := output.Stat()
	if err != nil || stat.Size() != 0 {
		t.Fatalf("stdout: %v %v", stat, err)
	}
}
