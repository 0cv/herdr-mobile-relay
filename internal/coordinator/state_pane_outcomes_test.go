package coordinator

import (
	"slices"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/question"
)

type transitionCall struct {
	paneID   string
	agent    string
	project  string
	status   string
	revision int64
}

func recordTransitions(s *State) *[]transitionCall {
	calls := &[]transitionCall{}
	s.SetOnTransition(func(paneID, agent, project, status string, revision int64) {
		*calls = append(*calls, transitionCall{paneID, agent, project, status, revision})
	})
	return calls
}

func sessionAgent(terminalID, status string) *AgentState {
	return &AgentState{
		PaneID:      "p1",
		RawPaneID:   "p1",
		TerminalID:  terminalID,
		TabID:       "tab-1",
		WorkspaceID: "workspace-1",
		Agent:       "claude",
		Project:     "relay",
		Status:      status,
	}
}

func commitSession(s *State, terminalID, status string) {
	s.CommitInventory([]*AgentState{sessionAgent(terminalID, status)}, s.RevisionCounter())
}

func TestTransitionCallbackReportsPaneAgentProjectStatusAndRevision(t *testing.T) {
	tests := []struct {
		name  string
		setup func(s *State)
		act   func(s *State)
		want  string
	}{
		{
			name: "working from an inventory commit",
			act:  func(s *State) { commitSession(s, "t1", "working") },
			want: "working",
		},
		{
			name:  "working from an event",
			setup: func(s *State) { commitSession(s, "t1", "idle") },
			act:   func(s *State) { s.CommitEvent("p1", "working", 1000) },
			want:  "working",
		},
		{
			name:  "blocked from an event",
			setup: func(s *State) { commitSession(s, "t1", "working") },
			act:   func(s *State) { s.CommitEvent("p1", "blocked", 1000) },
			want:  "blocked",
		},
		{
			name:  "completion arriving as done",
			setup: func(s *State) { commitSession(s, "t1", "working") },
			act:   func(s *State) { s.CommitEvent("p1", "done", 1000) },
			want:  "done",
		},
		{
			name:  "completion arriving as idle",
			setup: func(s *State) { commitSession(s, "t1", "working") },
			act:   func(s *State) { s.CommitEvent("p1", "idle", 1000) },
			want:  "idle",
		},
		{
			name: "blocked pane whose classification changes",
			setup: func(s *State) {
				commitSession(s, "t1", "working")
				s.CommitEvent("p1", "blocked", 1000)
			},
			act: func(s *State) {
				agent := sessionAgent("t1", "blocked")
				agent.AttentionKind = question.AttentionApproval
				s.CommitInventory([]*AgentState{agent}, s.RevisionCounter())
			},
			want: "blocked",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := testState()
			if test.setup != nil {
				test.setup(s)
			}
			calls := recordTransitions(s)

			test.act(s)

			want := []transitionCall{{
				paneID:   "p1",
				agent:    "claude",
				project:  "relay",
				status:   test.want,
				revision: s.Revision("p1"),
			}}
			if !slices.Equal(*calls, want) {
				t.Fatalf("transition calls = %+v, want %+v", *calls, want)
			}
		})
	}
}

func TestPaneLeavesAndReturnsWithSameSession(t *testing.T) {
	tests := []struct {
		name           string
		acknowledge    bool
		returnTerminal string
		wantDisplayed  string
	}{
		{name: "completion not acknowledged", returnTerminal: "t1", wantDisplayed: "done"},
		{name: "completion acknowledged", acknowledge: true, returnTerminal: "t1", wantDisplayed: "idle"},
		{name: "new terminal", returnTerminal: "t2", wantDisplayed: "idle"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := testState()
			commitSession(s, "t1", "working")
			s.CommitEvent("p1", "idle", 1000)
			completionRevision := s.Revision("p1")
			if !s.RegisterFinishedNotificationForTransition("p1", completionRevision) {
				t.Fatal("finished notification did not register for the completion")
			}
			if test.acknowledge {
				s.AcknowledgePane("p1")
			}
			s.RecordCustomAnswer("p1", "Which file?", "main.go")

			s.CommitInventory(nil, s.RevisionCounter())
			if generation := s.Generation("p1"); generation != 1 {
				t.Fatalf("generation after leaving = %d, want 1", generation)
			}
			commitSession(s, test.returnTerminal, "idle")

			if generation := s.Generation("p1"); generation != 2 {
				t.Errorf("generation after returning = %d, want 2", generation)
			}
			fresh := testState()
			commitSession(fresh, test.returnTerminal, "idle")
			if got, want := s.ContentRevision("p1"), fresh.ContentRevision("p1"); got != want {
				t.Errorf("content revision = %d, want %d as for a fresh pane", got, want)
			}
			if got, want := s.AttentionRevision("p1"), fresh.AttentionRevision("p1"); got != want {
				t.Errorf("attention revision = %d, want %d as for a fresh pane", got, want)
			}
			if answers := s.CustomAnswers("p1"); answers != nil {
				t.Errorf("custom answers survived leaving: %#v", answers)
			}
			if got := s.DisplayedStatus("p1"); got != test.wantDisplayed {
				t.Errorf("displayed status = %q, want %q", got, test.wantDisplayed)
			}
			if s.CompletionCurrent("p1", completionRevision) {
				t.Error("completion from before leaving is still current")
			}
			if s.RegisterFinishedNotificationForTransition("p1", completionRevision) {
				t.Error("old completion registered a notification after returning")
			}

			s.CommitEvent("p1", "working", 2000)
			s.CommitEvent("p1", "idle", 3000)
			if !s.RegisterFinishedNotificationForTransition("p1", s.Revision("p1")) {
				t.Error("new completion could not register a finished notification")
			}
		})
	}
}

func TestSamePaneIDReplacementResetsCompletionAndKeepsCounters(t *testing.T) {
	t.Run("unseen completion", func(t *testing.T) {
		s := testState()
		commitSession(s, "t1", "working")
		s.CommitEvent("p1", "idle", 1000)
		completionRevision := s.Revision("p1")
		if !s.RegisterFinishedNotificationForTransition("p1", completionRevision) {
			t.Fatal("finished notification did not register for the completion")
		}
		if got := s.DisplayedStatus("p1"); got != "done" {
			t.Fatalf("displayed status before replacement = %q, want done", got)
		}
		revision, content, attention := s.Revision("p1"), s.ContentRevision("p1"), s.AttentionRevision("p1")
		s.RecordCustomAnswer("p1", "Which file?", "main.go")

		commitSession(s, "t2", "idle")

		if got := s.DisplayedStatus("p1"); got != "idle" {
			t.Errorf("displayed status after replacement = %q, want idle", got)
		}
		if s.CompletionCurrent("p1", completionRevision) {
			t.Error("old completion is still current")
		}
		if s.RegisterFinishedNotificationForTransition("p1", completionRevision) {
			t.Error("old completion registered a notification after replacement")
		}
		assertCountersAdvanced(t, s, revision, content, attention)
		if answers := s.CustomAnswers("p1"); len(answers) != 1 ||
			answers[question.SummaryKey("Which file?")] != "main.go" {
			t.Errorf("custom answers after replacement = %#v, want the recorded answer", answers)
		}
	})

	t.Run("acknowledged completion", func(t *testing.T) {
		s := testState()
		commitSession(s, "t1", "working")
		s.CommitEvent("p1", "done", 1000)
		completionRevision := s.Revision("p1")
		if !s.RegisterFinishedNotificationForTransition("p1", completionRevision) {
			t.Fatal("finished notification did not register for the completion")
		}
		s.AcknowledgePane("p1")
		if got := s.DisplayedStatus("p1"); got != "idle" {
			t.Fatalf("displayed status before replacement = %q, want idle", got)
		}
		revision, content, attention := s.Revision("p1"), s.ContentRevision("p1"), s.AttentionRevision("p1")

		commitSession(s, "t2", "done")

		if got := s.DisplayedStatus("p1"); got != "done" {
			t.Errorf("displayed status after replacement = %q, want done (acknowledgement cleared)", got)
		}
		if s.CompletionCurrent("p1", completionRevision) {
			t.Error("old completion is still current")
		}
		if s.RegisterFinishedNotificationForTransition("p1", completionRevision) {
			t.Error("old completion registered a notification after replacement")
		}
		assertCountersAdvanced(t, s, revision, content, attention)
	})
}

func assertCountersAdvanced(t *testing.T, s *State, revision, content, attention int64) {
	t.Helper()
	if got := s.Revision("p1"); got <= revision {
		t.Errorf("revision = %d, want more than %d", got, revision)
	}
	if got := s.ContentRevision("p1"); got != content+1 {
		t.Errorf("content revision = %d, want %d", got, content+1)
	}
	if got := s.AttentionRevision("p1"); got != attention+1 {
		t.Errorf("attention revision = %d, want %d", got, attention+1)
	}
}

func TestNewWorkingCycleResetsCompletion(t *testing.T) {
	s := testState()
	commitSession(s, "t1", "working")
	s.CommitEvent("p1", "done", 1000)
	completionRevision := s.Revision("p1")
	if !s.RegisterFinishedNotificationForTransition("p1", completionRevision) {
		t.Fatal("finished notification did not register for the first completion")
	}
	s.AcknowledgePane("p1")
	if got := s.DisplayedStatus("p1"); got != "idle" {
		t.Fatalf("displayed status after acknowledgement = %q, want idle", got)
	}

	s.CommitEvent("p1", "working", 2000)
	if s.CompletionCurrent("p1", completionRevision) {
		t.Error("first completion is still current while working")
	}
	s.CommitEvent("p1", "done", 3000)

	if got := s.DisplayedStatus("p1"); got != "done" {
		t.Errorf("displayed status of second completion = %q, want done (acknowledgement cleared)", got)
	}
	secondRevision := s.Revision("p1")
	if s.RegisterFinishedNotificationForTransition("p1", completionRevision) {
		t.Error("first completion registered a notification in the next cycle")
	}
	if !s.RegisterFinishedNotificationForTransition("p1", secondRevision) {
		t.Error("second completion could not register a finished notification")
	}
	if s.RegisterFinishedNotificationForTransition("p1", secondRevision) {
		t.Error("second completion registered twice")
	}

	s.CommitEvent("p1", "working", 4000)
	s.CommitEvent("p1", "idle", 5000)
	if got := s.DisplayedStatus("p1"); got != "done" {
		t.Errorf("displayed status of idle completion = %q, want done (unseen)", got)
	}
	if !s.RegisterFinishedNotificationForTransition("p1", s.Revision("p1")) {
		t.Error("third completion could not register a finished notification")
	}
}
