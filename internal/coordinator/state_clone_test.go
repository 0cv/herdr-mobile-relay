package coordinator

import (
	"reflect"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/question"
)

func TestAgentStateClone(t *testing.T) {
	var absent *AgentState
	if absent.Clone() != nil {
		t.Fatal("nil agent clone is not nil")
	}

	for _, agent := range []*AgentState{
		{PaneID: "pane-1", StateRevision: 42},
		{
			PaneID:        "pane-2",
			StateRevision: 43,
			Options:       []string{"approve"},
			Interaction: &question.Interaction{
				Question: "Continue?",
				Options: []question.Option{{
					Label:   "Yes",
					Summary: []question.SummaryEntry{{Question: "Scope?", Answer: "All"}},
				}},
			},
		},
	} {
		cloned := agent.Clone()
		if cloned == agent || !reflect.DeepEqual(cloned, agent) {
			t.Fatalf("clone = %#v, want independent copy of %#v", cloned, agent)
		}
		cloned.PaneID = "changed"
		if agent.PaneID == "changed" {
			t.Fatal("clone shares agent state")
		}
		if cloned.Interaction == nil {
			continue
		}
		cloned.Options[0] = "changed"
		cloned.Interaction.Question = "changed"
		cloned.Interaction.Options[0].Label = "changed"
		cloned.Interaction.Options[0].Summary[0].Answer = "changed"
		if agent.Options[0] != "approve" || agent.Interaction.Question != "Continue?" ||
			agent.Interaction.Options[0].Label != "Yes" || agent.Interaction.Options[0].Summary[0].Answer != "All" {
			t.Fatal("clone shares nested agent state")
		}
	}
}
