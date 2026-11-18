package coordinator

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/question"
)

func TestQuestionPaneReadDoesNotBlockIngressAdmission(t *testing.T) {
	questionView := "Which deployment target?\n❯ 1. Development\n2. Staging\n3. Type something.\n4. Chat about this\nEnter to select · ↑/↓ to navigate · Esc to cancel"
	interaction := question.Parse(questionView, "claude")
	if interaction == nil {
		t.Fatal("test question did not parse")
	}
	client := newFakeBackend()
	client.contents["pane-1"] = questionView
	releaseRead := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(releaseRead) }) }
	client.setHook("ReadPane", func(ctx context.Context, _ fakeCall) error {
		select {
		case <-releaseRead:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})

	dispatcher := newTestDispatcher(t, client, NewState(testLogger()))
	t.Cleanup(unblock)
	dispatcher.state.CommitInventory([]*AgentState{
		{PaneID: "pane-1", Agent: "claude", Status: "blocked"},
		{PaneID: "pane-2", Agent: "codex", Status: "working"},
	}, dispatcher.state.RevisionCounter())

	admitted := make(chan struct{})
	questionDone := make(chan *CommandResult, 1)
	go func() {
		questionDone <- dispatcher.HandleAdmitted(t.Context(), protocol.Inbound{
			Type:            "answer_question",
			RequestID:       "question",
			PaneID:          "pane-1",
			InteractionID:   interaction.ID,
			SelectedIndices: []int{0},
		}, func() { close(admitted) })
	}()

	select {
	case <-admitted:
	case <-time.After(time.Second):
		t.Fatal("question work was not admitted before its pane read completed")
	}
	select {
	case result := <-questionDone:
		t.Fatalf("question completed before the blocked pane read was released: %+v", result)
	default:
	}

	promptCtx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	prompt := dispatcher.Handle(promptCtx, protocol.Inbound{
		Type:      "submit_prompt",
		RequestID: "prompt",
		PaneID:    "pane-2",
		Text:      "continue",
	})
	if !prompt.OK {
		t.Fatalf("unrelated pane command was blocked behind question validation: %+v", prompt)
	}

	unblock()
	select {
	case result := <-questionDone:
		if !result.OK {
			t.Fatalf("question result = %+v", result)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("question did not complete after pane read was released")
	}
}
