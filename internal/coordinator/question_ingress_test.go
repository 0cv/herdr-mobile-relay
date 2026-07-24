package coordinator

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/question"
)

func TestQuestionPaneReadDoesNotBlockIngressAdmission(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "read-started")
	releaseRead := filepath.Join(dir, "release-read")
	questionView := "Which deployment target?\n❯ 1. Development\n2. Staging\n3. Type something.\n4. Chat about this\nEnter to select · ↑/↓ to navigate · Esc to cancel"
	interaction := question.Parse(questionView, "claude")
	if interaction == nil {
		t.Fatal("test question did not parse")
	}
	bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"if [ \"$1 $2 $3\" = \"pane read pane-1\" ]; then\n"+
		"  touch \""+started+"\"\n"+
		"  while [ ! -f \""+releaseRead+"\" ]; do sleep 0.01; done\n"+
		"  printf '%s\\n' '"+questionView+"'\n"+
		"else\n"+
		"  printf '{\"ok\":true}\\n'\n"+
		"fi\n")

	dispatcher := NewDispatcher(
		herdr.NewClient(bin, filepath.Join(dir, "sock")),
		NewState(testLogger()),
		nil,
		testLogger(),
	)
	t.Cleanup(func() {
		_ = os.WriteFile(releaseRead, nil, 0o600)
		_ = dispatcher.Close(context.Background())
	})
	dispatcher.state.CommitInventory([]*AgentState{
		{PaneID: "pane-1", Agent: "claude", Status: "blocked"},
		{PaneID: "pane-2", Agent: "codex", Status: "working"},
	}, dispatcher.state.RevisionCounter())

	admitted := make(chan struct{})
	questionDone := make(chan *CommandResult, 1)
	go func() {
		questionDone <- dispatcher.HandleAdmitted(t.Context(), map[string]any{
			"action":           "answer_question",
			"request_id":       "question",
			"pane_id":          "pane-1",
			"interaction_id":   interaction.ID,
			"selected_indices": []any{float64(0)},
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

	promptCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	prompt := dispatcher.Handle(promptCtx, map[string]any{
		"action":     "submit_prompt",
		"request_id": "prompt",
		"pane_id":    "pane-2",
		"text":       "continue",
	})
	if !prompt.OK {
		t.Fatalf("unrelated pane command was blocked behind question validation: %+v", prompt)
	}

	if err := os.WriteFile(releaseRead, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-questionDone:
		if !result.OK {
			t.Fatalf("question result = %+v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("question did not complete after pane read was released")
	}
}
