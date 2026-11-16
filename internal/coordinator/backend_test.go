package coordinator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/question"
)

func TestBackendPromptSubmitsWithoutManualInput(t *testing.T) {
	client := newFakeBackend()
	client.status = herdr.ServerStatus{}
	dispatcher := newTestDispatcher(t, client, NewState(testLogger()))
	if err := dispatcher.deliverPrompt(context.Background(), WorkerToken{PaneID: "pane-1", AllowAbsent: true}, "answer", true); err != nil {
		t.Fatal(err)
	}
	if client.count("Prompt") != 1 || client.count("SendText") != 0 || client.count("SendKeys") != 0 {
		t.Fatalf("calls: %+v", client.recorded())
	}
}

func TestBackendRefusesQuestionTextBeforeSendingInput(t *testing.T) {
	client := newFakeBackend()
	client.status = herdr.ServerStatus{}
	dispatcher := newTestDispatcher(t, client, NewState(testLogger()))
	err := dispatcher.executeQuestion(context.Background(), WorkerToken{
		PaneID: "pane-1", AllowAbsent: true,
	}, "pane-1", questionPayload{
		OtherSelected: true, OtherText: "A custom answer",
	}, &question.Interaction{
		Agent: "codex", Kind: "single_select", AllOptionCount: 2,
		Focus: question.Focus{Kind: "option", Index: 0},
	})
	if !errors.Is(err, errQuestionTextUnsupported) {
		t.Fatalf("error = %v, want unsupported typed text refusal", err)
	}
	if errors.Is(err, herdr.ErrPartiallyApplied) || errors.Is(err, herdr.ErrDispatchedUnknown) {
		t.Fatalf("refusal classified as applied: %v", err)
	}
	if client.count("SendKeys") != 0 || client.count("SendText") != 0 {
		t.Fatalf("backend received %d key calls and %d text calls", client.count("SendKeys"), client.count("SendText"))
	}
}

func TestBackendRefusesUnsupportedAgentLaunch(t *testing.T) {
	for _, action := range []string{"agent_start", "agent_clear"} {
		t.Run(action, func(t *testing.T) {
			client := newFakeBackend()
			client.status = herdr.ServerStatus{}
			dispatcher := newTestDispatcher(t, client, NewState(testLogger()))
			result := dispatcher.Handle(context.Background(), protocol.Inbound{
				Type:   action,
				PaneID: "pane-1",
			})
			if result.OK || result.Error != "This backend does not support launching agents" {
				t.Fatalf("result = %+v", result)
			}
			if len(client.recorded()) != 0 {
				t.Fatalf("unsupported launch caused backend calls: %+v", client.recorded())
			}
		})
	}
}

func TestPromptNotStartedKeepsAction(t *testing.T) {
	for _, reason := range []string{"expired", "closed"} {
		t.Run(reason, func(t *testing.T) {
			client := newFakeBackend()
			state := NewState(testLogger())
			state.CommitInventory([]*AgentState{{PaneID: "pane-1", Agent: "codex"}}, state.RevisionCounter())
			dispatcher := newTestDispatcher(t, client, state)
			message := protocol.Inbound{
				Type:      "submit_prompt",
				RequestID: "prompt",
				PaneID:    "pane-1",
				Text:      "hello",
			}
			if reason == "expired" {
				message.ReceivedAt = time.Now().Add(-2 * commandDeadline)
			} else if err := dispatcher.scheduler.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			result := dispatcher.Handle(context.Background(), message)
			if result.OK || result.Phase != "not_started" || result.Action != "submit_prompt" {
				t.Fatalf("prompt result = %+v, want not-started submit_prompt", result)
			}
			if client.count("Prompt") != 0 {
				t.Fatal("prompt reached the backend")
			}
		})
	}
}

func TestBackendTerminalReplacementInvalidatesQueuedTarget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := newFakeBackend()
	client.panes = []herdr.Pane{{ID: "pane-1", TerminalID: "terminal-1", Agent: "codex"}}
	state := NewState(testLogger())
	dispatcher := newTestDispatcher(t, client, state)
	poller := NewPoller(client, state, time.Second, testLogger())
	poller.SetOnInventoryChange(func() error {
		dispatcher.PruneSlots(map[string]bool{"pane-1": true})
		return nil
	})
	poller.poll(ctx)
	generation := state.Generation("pane-1")
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	go func() {
		now := time.Now()
		_, _ = dispatcher.scheduler.Execute(ctx, ScheduleOptions{Command: Command{
			ID: dispatcher.scheduler.NextCommandID(), RequestID: "first", ReceivedAt: now,
			Deadline: now.Add(5 * time.Second), Action: "submit_prompt", PaneID: "pane-1",
		}, PaneGeneration: generation}, EffectFunc(func(context.Context, WorkerToken) EffectResult {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
			}
			return EffectResult{Result: completed("first", "prompt", "pane-1", nil)}
		}))
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	admitted := make(chan struct{})
	results := make(chan *CommandResult, 1)
	go func() {
		results <- dispatcher.HandleAdmitted(ctx, protocol.Inbound{
			Type:      "submit_prompt",
			RequestID: "queued",
			PaneID:    "pane-1",
			Text:      "hello",
		}, func() { close(admitted) })
	}()
	select {
	case <-admitted:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	client.update(func(b *fakeBackend) { b.panes[0].TerminalID = "terminal-2" })
	poller.poll(ctx)
	if got := state.Generation("pane-1"); got != generation+1 {
		t.Fatalf("generation = %d, want %d", got, generation+1)
	}
	select {
	case result := <-results:
		if result == nil || result.OK || result.Error != "pane session was replaced" || result.Action != "submit_prompt" {
			t.Fatalf("queued target result = %+v, want stale submit_prompt failure", result)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if client.count("GetInventory") != 2 || client.count("Prompt") != 0 {
		t.Fatalf("inventories = %d, prompts = %d", client.count("GetInventory"), client.count("Prompt"))
	}
}
