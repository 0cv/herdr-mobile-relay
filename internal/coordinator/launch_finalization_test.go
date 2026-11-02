package coordinator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAgentStartRetryReplaysInitialPromptWarning(t *testing.T) {
	f := newLaunchFixtureWithOptions(t, launchFixtureOptions{prompt: "exit 1"})
	d, record := f.dispatcher, f.calls
	message := map[string]any{"action": "agent_start", "request_id": "warning", "profile_id": "claude", "name": "proj", "cwd": f.cwd, "prompt": "hello"}
	first, second := d.Handle(f.ctx, message), d.Handle(f.ctx, message)
	f.assertCalls(t, 1)
	if !second.replayed || first.Phase != "completed_with_warning" || second.Phase != first.Phase {
		t.Fatalf("results = %+v, %+v", first, second)
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "agent start") != 1 || strings.Count(string(data), "agent prompt") != 1 {
		t.Fatalf("calls = %s", data)
	}
}

func TestLateLaunchReceivesFreshPromptBudget(t *testing.T) {
	f := newLaunchFixture(t)
	d, record := f.dispatcher, f.calls
	result := d.handleAgentStart(f.ctx, time.Now().Add(-13*time.Second), "late", map[string]any{"profile_id": "claude", "name": "proj", "cwd": f.cwd, "prompt": "hello"})
	f.assertLaunch(t, result)
	f.assertCalls(t, 1)
	if result.Phase != "completed" {
		t.Fatalf("result = %+v", result)
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "agent prompt") != 1 {
		t.Fatalf("calls = %s", data)
	}
}

func TestInitialQoderPromptUsesTextAndEnter(t *testing.T) {
	for _, failEnter := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "partial"}[failEnter], func(t *testing.T) {
			enter := ""
			if failEnter {
				enter = "exit 1"
			}
			f := newLaunchFixtureWithOptions(t, launchFixtureOptions{enter: enter})
			d, record := f.dispatcher, f.calls
			message := map[string]any{"action": "agent_start", "request_id": "qoder", "profile_id": "qodercli", "name": "proj", "cwd": f.cwd, "prompt": "hello"}
			result := d.Handle(f.ctx, message)
			want := "completed"
			if failEnter {
				want = "completed_with_warning"
			}
			if result.Phase != want {
				t.Fatalf("result = %+v", result)
			}
			replay := d.Handle(f.ctx, message)
			if !replay.replayed || replay.Phase != want {
				t.Fatalf("replay = %+v", replay)
			}
			data, err := os.ReadFile(record)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(data), "workspace create") != 1 || strings.Count(string(data), "agent start") != 1 || strings.Count(string(data), "pane send-text") != 1 || strings.Count(string(data), "pane send-keys") != 1 || strings.Contains(string(data), "agent prompt") {
				t.Fatalf("calls = %s", data)
			}
		})
	}
}

func TestInitialPromptBlocksDuplicateCompletion(t *testing.T) {
	dir := t.TempDir()
	entered, release := filepath.Join(dir, "entered"), filepath.Join(dir, "release")
	f := newLaunchFixtureWithOptions(t, launchFixtureOptions{prompt: fmt.Sprintf(": > %q; while [ ! -f %q ]; do /bin/sleep 0.01; done; exit 1", entered, release)})
	d := f.dispatcher
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0600) })
	message := map[string]any{"action": "agent_start", "request_id": "blocked", "profile_id": "claude", "name": "proj", "cwd": f.cwd, "prompt": "hello"}
	first, duplicate := make(chan *CommandResult, 1), make(chan *CommandResult, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	go func() { first <- d.Handle(ctx, message) }()
	for {
		if _, err := os.Stat(entered); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("prompt did not start")
		case <-time.After(time.Millisecond):
		}
	}
	go func() { duplicate <- d.Handle(ctx, message) }()
	select {
	case result := <-duplicate:
		t.Fatalf("premature result = %+v", result)
	case <-time.After(50 * time.Millisecond):
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, ch := range []chan *CommandResult{first, duplicate} {
		select {
		case result := <-ch:
			if result.Phase != "completed_with_warning" {
				t.Fatalf("result = %+v", result)
			}
		case <-ctx.Done():
			t.Fatal("result did not complete")
		}
	}
	f.assertCalls(t, 1)
}
