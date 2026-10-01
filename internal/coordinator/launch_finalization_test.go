package coordinator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func TestAgentStartRetryReplaysInitialPromptWarning(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "calls")
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \""+record+"\"\ncase \"$1 $2\" in\n 'agent start') printf '%s' '{\"result\":{\"pane_id\":\"pane-new\"}}' ;;\n 'agent prompt') exit 1 ;;\nesac\n")
	d := NewDispatcher(herdr.NewClient(bin, filepath.Join(dir, "sock")), NewState(testLogger()), nil, testLogger())
	t.Cleanup(func() { _ = d.Close(context.Background()) })
	message := map[string]any{"action": "agent_start", "request_id": "warning", "profile_id": "claude", "name": "proj", "cwd": "/tmp", "prompt": "hello"}
	first, second := d.Handle(context.Background(), message), d.Handle(context.Background(), message)
	if first.Phase != "completed_with_warning" || second.Phase != first.Phase {
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
	dir := t.TempDir()
	record := filepath.Join(dir, "calls")
	bin := recordingHerdr(t, dir, record, `{"result":{"pane_id":"pane-new"}}`)
	d := NewDispatcher(herdr.NewClient(bin, filepath.Join(dir, "sock")), NewState(testLogger()), nil, testLogger())
	t.Cleanup(func() { _ = d.Close(context.Background()) })
	result := d.handleAgentStart(context.Background(), time.Now().Add(-13*time.Second), "late", map[string]any{"profile_id": "claude", "name": "proj", "cwd": "/tmp", "prompt": "hello"})
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
			dir := t.TempDir()
			record := filepath.Join(dir, "calls")
			exit := "0"
			if failEnter {
				exit = "1"
			}
			bin := writeScript(t, dir, "herdr", "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \""+record+"\"\ncase \"$1 $2\" in\n 'agent start') printf '%s' '{\"result\":{\"pane_id\":\"pane-new\"}}' ;;\n 'pane send-keys') exit "+exit+" ;;\nesac\n")
			d := NewDispatcher(herdr.NewClient(bin, filepath.Join(dir, "sock")), NewState(testLogger()), nil, testLogger())
			t.Cleanup(func() { _ = d.Close(context.Background()) })
			message := map[string]any{"action": "agent_start", "request_id": "qoder", "profile_id": "qoder", "name": "proj", "cwd": "/tmp", "prompt": "hello"}
			result := d.Handle(context.Background(), message)
			want := "completed"
			if failEnter {
				want = "completed_with_warning"
			}
			if result.Phase != want {
				t.Fatalf("result = %+v", result)
			}
			replay := d.Handle(context.Background(), message)
			if replay.Phase != want {
				t.Fatalf("replay = %+v", replay)
			}
			data, err := os.ReadFile(record)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(data), "pane send-text") != 1 || strings.Count(string(data), "pane send-keys") != 1 || strings.Contains(string(data), "agent prompt") {
				t.Fatalf("calls = %s", data)
			}
		})
	}
}

func TestInitialPromptBlocksDuplicateCompletion(t *testing.T) {
	dir := t.TempDir()
	entered, release := filepath.Join(dir, "entered"), filepath.Join(dir, "release")
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\ncase \"$1 $2\" in\n 'agent start') printf '%s' '{\"result\":{\"pane_id\":\"pane-new\"}}' ;;\n 'agent prompt') touch \""+entered+"\"; while [ ! -f \""+release+"\" ]; do sleep 0.01; done; exit 1 ;;\nesac\n")
	d := NewDispatcher(herdr.NewClient(bin, filepath.Join(dir, "sock")), NewState(testLogger()), nil, testLogger())
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0600); _ = d.Close(context.Background()) })
	message := map[string]any{"action": "agent_start", "request_id": "blocked", "profile_id": "claude", "name": "proj", "cwd": "/tmp", "prompt": "hello"}
	first, duplicate := make(chan *CommandResult, 1), make(chan *CommandResult, 1)
	go func() { first <- d.Handle(context.Background(), message) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(entered); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("prompt did not start")
		}
		time.Sleep(time.Millisecond)
	}
	go func() { duplicate <- d.Handle(context.Background(), message) }()
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
		case <-time.After(3 * time.Second):
			t.Fatal("result did not complete")
		}
	}
}
