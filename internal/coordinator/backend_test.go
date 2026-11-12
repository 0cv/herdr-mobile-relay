package coordinator

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/backend"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/question"
)

type recordingBackend struct {
	backend.Backend
	pane          herdr.Pane
	inventories   int
	prompts       atomic.Int32
	launchEffects atomic.Int32
	keys          atomic.Int32
	texts         atomic.Int32
}

func (b *recordingBackend) GetInventory(context.Context) (*herdr.Inventory, error) {
	b.inventories++
	return &herdr.Inventory{Panes: []herdr.Pane{b.pane}}, nil
}

func (b *recordingBackend) PaneList(context.Context) ([]herdr.Pane, error) {
	return []herdr.Pane{b.pane}, nil
}

func (b *recordingBackend) WorkspaceList(context.Context) ([]herdr.Workspace, error) {
	return nil, nil
}

func (b *recordingBackend) TabList(context.Context) ([]herdr.Tab, error) {
	return nil, nil
}

func (b *recordingBackend) Prompt(context.Context, string, string) error {
	b.prompts.Add(1)
	return nil
}

func (b *recordingBackend) SendKeys(context.Context, string, []string) error {
	b.keys.Add(1)
	return nil
}

func (b *recordingBackend) SendText(context.Context, string, string) error {
	b.texts.Add(1)
	return nil
}

func TestBackendPromptSubmitsWithoutManualInput(t *testing.T) {
	client := &recordingBackend{}
	dispatcher := NewDispatcher(client, NewState(testLogger()), nil, testLogger())
	defer dispatcher.Close(context.Background())
	if err := dispatcher.deliverPrompt(context.Background(), WorkerToken{PaneID: "pane-1", AllowAbsent: true}, "answer", true); err != nil {
		t.Fatal(err)
	}
	if client.prompts.Load() != 1 || client.texts.Load() != 0 || client.keys.Load() != 0 {
		t.Fatalf("calls: prompt=%d text=%d keys=%d", client.prompts.Load(), client.texts.Load(), client.keys.Load())
	}
}

func TestBackendRefusesQuestionTextBeforeSendingInput(t *testing.T) {
	client := &recordingBackend{}
	dispatcher := NewDispatcher(client, NewState(testLogger()), nil, testLogger())
	defer dispatcher.Close(context.Background())
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
	if client.keys.Load() != 0 || client.texts.Load() != 0 {
		t.Fatalf("backend received %d key calls and %d text calls", client.keys.Load(), client.texts.Load())
	}
}

func (b *recordingBackend) CapabilityStatus() herdr.ServerStatus {
	return herdr.ServerStatus{}
}

func (b *recordingBackend) StartAgent(context.Context, string, string, string, int, ...string) (string, error) {
	b.launchEffects.Add(1)
	return "", nil
}

func (b *recordingBackend) PaneRun(context.Context, string, []string) error {
	b.launchEffects.Add(1)
	return nil
}

func (b *recordingBackend) StopPane(context.Context, string) error {
	b.launchEffects.Add(1)
	return nil
}

func TestBackendRefusesUnsupportedAgentLaunch(t *testing.T) {
	for _, action := range []string{"agent_start", "agent_clear", "agent_restart"} {
		t.Run(action, func(t *testing.T) {
			client := &recordingBackend{}
			dispatcher := NewDispatcher(client, NewState(testLogger()), nil, testLogger())
			defer dispatcher.Close(context.Background())
			result := dispatcher.Handle(context.Background(), map[string]any{
				"type": action, "pane_id": "pane-1",
			})
			if result.OK || result.Error != "This backend does not support launching agents" {
				t.Fatalf("result = %+v", result)
			}
			if client.launchEffects.Load() != 0 {
				t.Fatal("unsupported launch caused backend effects")
			}
		})
	}
}

func TestPromptNotStartedKeepsAction(t *testing.T) {
	for _, reason := range []string{"expired", "closed"} {
		t.Run(reason, func(t *testing.T) {
			client := &recordingBackend{}
			state := NewState(testLogger())
			state.CommitInventory([]*AgentState{{PaneID: "pane-1", Agent: "codex"}}, state.RevisionCounter())
			dispatcher := NewDispatcher(client, state, nil, testLogger())
			defer dispatcher.Close(context.Background())
			message := map[string]any{
				"action": "submit_prompt", "request_id": "prompt", "pane_id": "pane-1", "text": "hello",
			}
			if reason == "expired" {
				message["_server_received_at"] = time.Now().Add(-2 * commandDeadline).Format(time.RFC3339Nano)
			} else if err := dispatcher.scheduler.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			result := dispatcher.Handle(context.Background(), message)
			if result.OK || result.Phase != "not_started" || result.Action != "submit_prompt" {
				t.Fatalf("prompt result = %+v, want not-started submit_prompt", result)
			}
			if client.prompts.Load() != 0 {
				t.Fatal("prompt reached the backend")
			}
		})
	}
}

func TestBackendTerminalReplacementInvalidatesQueuedTarget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := &recordingBackend{pane: herdr.Pane{ID: "pane-1", TerminalID: "terminal-1", Agent: "codex"}}
	state := NewState(testLogger())
	dispatcher := NewDispatcher(client, state, nil, testLogger())
	defer dispatcher.Close(ctx)
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
		}, PaneGeneration: uint64(generation)}, EffectFunc(func(context.Context, WorkerToken) EffectResult {
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
		results <- dispatcher.HandleAdmitted(ctx, map[string]any{
			"action": "submit_prompt", "request_id": "queued", "pane_id": "pane-1", "text": "hello",
		}, func() { close(admitted) })
	}()
	select {
	case <-admitted:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	client.pane.TerminalID = "terminal-2"
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
	if client.inventories != 2 || client.prompts.Load() != 0 {
		t.Fatalf("inventories = %d, prompts = %d", client.inventories, client.prompts.Load())
	}
}
