package coordinator

import (
	"context"
	"testing"
	"time"
)

func TestAgentStartRetryReplaysInitialPromptWarning(t *testing.T) {
	f := newLaunchFixtureWithOptions(t, launchFixtureOptions{prompt: failCall("exit status 1")})
	d := f.dispatcher
	message := map[string]any{"type": "agent_start", "request_id": "warning", "profile_id": "claude", "name": "proj", "cwd": f.cwd, "prompt": "hello"}
	first, second := d.Handle(f.ctx, message), d.Handle(f.ctx, message)
	f.assertCalls(t, 1)
	if !second.replayed || first.Phase != "completed_with_warning" || second.Phase != first.Phase {
		t.Fatalf("results = %+v, %+v", first, second)
	}
	if f.client.count("StartAgent") != 1 || f.client.count("Prompt") != 1 {
		t.Fatalf("calls = %+v", f.client.recorded())
	}
}

func TestLateLaunchReceivesFreshPromptBudget(t *testing.T) {
	f := newLaunchFixture(t)
	d := f.dispatcher
	result := d.handleAgentStart(f.ctx, d.newCommandRequest(time.Now().Add(-13*time.Second), "late", "", map[string]any{"profile_id": "claude", "name": "proj", "cwd": f.cwd, "prompt": "hello"}, nil))
	f.assertLaunch(t, result)
	f.assertCalls(t, 1)
	if result.Phase != "completed" {
		t.Fatalf("result = %+v", result)
	}
	if f.client.count("Prompt") != 1 {
		t.Fatalf("calls = %+v", f.client.recorded())
	}
}

func TestInitialQoderPromptUsesTextAndEnter(t *testing.T) {
	for _, failEnter := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "partial"}[failEnter], func(t *testing.T) {
			var enter fakeHook
			if failEnter {
				enter = failCall("exit status 1")
			}
			f := newLaunchFixtureWithOptions(t, launchFixtureOptions{enter: enter})
			d := f.dispatcher
			message := map[string]any{"type": "agent_start", "request_id": "qoder", "profile_id": "qodercli", "name": "proj", "cwd": f.cwd, "prompt": "hello"}
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
			client := f.client
			if client.count("WorkspaceCreate") != 1 || client.count("StartAgent") != 1 || client.count("SendText") != 1 || client.count("SendKeys") != 1 || client.count("Prompt") != 0 {
				t.Fatalf("calls = %+v", client.recorded())
			}
		})
	}
}

func TestInitialPromptBlocksDuplicateCompletion(t *testing.T) {
	gate := newReleaseGate()
	f := newLaunchFixtureWithOptions(t, launchFixtureOptions{prompt: gate.hook(failCall("exit status 1"))})
	d := f.dispatcher
	t.Cleanup(gate.open)
	message := map[string]any{"type": "agent_start", "request_id": "blocked", "profile_id": "claude", "name": "proj", "cwd": f.cwd, "prompt": "hello"}
	first, duplicate := make(chan *CommandResult, 1), make(chan *CommandResult, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	go func() { first <- d.Handle(ctx, message) }()
	select {
	case <-gate.entered:
	case <-ctx.Done():
		t.Fatal("prompt did not start")
	}
	go func() { duplicate <- d.Handle(ctx, message) }()
	select {
	case result := <-duplicate:
		t.Fatalf("premature result = %+v", result)
	case <-time.After(50 * time.Millisecond):
	}
	gate.open()
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
