package coordinator

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/question"
)

func probeAgent(status string) *AgentState {
	return &AgentState{PaneID: "pane", TabID: "tab", WorkspaceID: "workspace", TerminalID: "terminal", SessionID: "session", Agent: "claude", Status: status}
}

func probeState(t *testing.T, initial *AgentState) (*State, *[]string) {
	t.Helper()
	s := NewState(nil)
	s.EnableIdleQuestionProbe()
	var transitions []string
	s.SetOnTransition(func(_, _, _, status string, _ int64) { transitions = append(transitions, status) })
	s.CommitInventory([]*AgentState{initial}, s.RevisionCounter())
	return s, &transitions
}

func captureProbe(t *testing.T, s *State, leased bool) ProbeToken {
	t.Helper()
	a, _ := s.Agent("pane")
	token, _, ok := s.ProbeToken(a.ProbeTarget(), leased)
	if !ok {
		t.Fatalf("no probe for %+v", a)
	}
	return token
}

func questionHit(id string) question.Classification {
	return question.Classification{Kind: question.AttentionQuestion, Prompt: "Which?", Command: "Which?", Interaction: &question.Interaction{ID: id}}
}

func approvalHit(id string) question.Classification {
	return question.Classification{Kind: question.AttentionApproval, Prompt: "Allow?", Command: "Allow?", Options: []string{"Yes", "No"}, ApprovalIdentity: id}
}

func TestProbePendingStops(t *testing.T) {
	for _, tc := range []struct {
		initial, agent, raw string
		enabled, pending    bool
	}{
		{"working", "claude", "idle", true, true},
		{"working", "claude", "done", true, true},
		{"idle", "claude", "idle", true, false},
		{"working", "claude", "idle", false, false},
		{"working", "unknown", "idle", true, false},
		{"blocked", "claude", "blocked", true, false},
	} {
		t.Run(fmt.Sprint(tc), func(t *testing.T) {
			s := NewState(nil)
			if tc.enabled {
				s.EnableIdleQuestionProbe()
			}
			a := probeAgent(tc.initial)
			a.Agent = tc.agent
			if tc.initial == "blocked" {
				applyAttentionClassification(a, questionHit("native"))
			}
			s.CommitInventory([]*AgentState{a}, 0)
			s.CommitEventForSession("pane", "tab", "workspace", tc.raw, 1234)
			got, _ := s.Agent("pane")
			want := tc.raw
			if tc.pending {
				want = tc.initial
			}
			if got.ProbePending != tc.pending || got.RawStatus != tc.raw || got.Status != want || tc.initial == "blocked" && got.InteractionID != "native" {
				t.Fatalf("unexpected state %+v", got)
			}
			_, _, eligible := s.ProbeToken(got.ProbeTarget(), false)
			if eligible != tc.pending {
				t.Fatalf("eligible=%t", eligible)
			}
		})
	}
}

func TestProbeDismissalUsesSuccessfulSeparatedMisses(t *testing.T) {
	s, transitions := probeState(t, probeAgent("working"))
	now := time.Unix(123, 0)
	s.CommitEventForSession("pane", "tab", "workspace", "idle", 1)
	first := captureProbe(t, s, false)
	s.CommitEventForSession("pane", "tab", "workspace", "done", 2)
	if !s.CommitProbe(first, questionHit("A"), true, false, now) {
		t.Fatal("initial promotion rejected")
	}
	pinned, _ := s.Agent("pane")
	commit := func(offset time.Duration, c question.Classification, reliable bool) bool {
		return s.CommitProbe(captureProbe(t, s, false), c, reliable, false, now.Add(offset))
	}
	if commit(0, question.Classification{}, false) || commit(time.Second, question.Classification{}, true) {
		t.Fatal("premature dismissal")
	}
	s.CommitEventForSession("pane", "tab", "workspace", "idle", 3)
	s.CommitEventForSession("pane", "tab", "workspace", "done", 4)
	still, _ := s.Agent("pane")
	if still.RawStatus != "done" || still.Status != "blocked" || still.BlockedEventID != pinned.BlockedEventID || still.InteractionID != "A" {
		t.Fatalf("UDP lost pin: %+v", still)
	}
	if commit(2*time.Second, question.Classification{}, true) {
		t.Fatal("misses were too close")
	}
	if commit(3*time.Second, questionHit("A"), true) || commit(4*time.Second, question.Classification{}, true) || commit(6*time.Second, question.Classification{}, false) {
		t.Fatal("hit or failed read changed the pin")
	}
	if !commit(7*time.Second, question.Classification{}, true) {
		t.Fatal("misses should have ended wait")
	}
	end, _ := s.Agent("pane")
	if end.Status != "done" || end.ScreenAttention != ScreenCleared || end.ProbePending || end.Interaction != nil || end.BlockedEventID != "" || !s.CompletionCurrent("pane", end.StateRevision) {
		t.Fatalf("incorrect completion: %+v", end)
	}
	if !reflect.DeepEqual(*transitions, []string{"working", "blocked", "done"}) {
		t.Fatal(*transitions)
	}
}

func TestProbeReplacementDiscardsOldIdentity(t *testing.T) {
	for _, topology := range []bool{false, true} {
		for _, replacement := range []string{"terminal", "session", "disappearance", "bump"} {
			t.Run(fmt.Sprintf("%t/%s", topology, replacement), func(t *testing.T) {
				s, transitions := probeState(t, probeAgent("working"))
				now := time.Unix(123, 0)
				s.CommitEvent("pane", "idle", 1)
				s.CommitProbe(captureProbe(t, s, false), questionHit("A"), true, false, now)
				s.CommitProbe(captureProbe(t, s, false), question.Classification{}, true, false, now)
				inFlight := captureProbe(t, s, false)
				commit := func(list []*AgentState) {
					if topology {
						s.CommitTopology(list, nil, s.RevisionCounter())
						return
					}
					if _, ok := s.CommitPoll(list, nil, s.BeginPoll()); !ok {
						t.Fatal("poll rejected")
					}
				}
				next := probeAgent("idle")
				switch replacement {
				case "terminal":
					next.TerminalID = "new"
				case "session":
					next.SessionID = "new"
				case "disappearance":
					commit(nil)
				case "bump":
					s.BumpGeneration("pane")
				}
				commit([]*AgentState{next})
				if s.CommitProbe(inFlight, questionHit("B"), true, true, now) {
					t.Fatal("obsolete read applied")
				}
				got, _ := s.Agent("pane")
				if got.Generation == inFlight.target.Generation || got.RawStatus != "idle" || got.Status != "idle" || got.ProbePending || got.BlockedEventID != "" || got.ScreenAttention != "" || got.Interaction != nil {
					t.Fatalf("inherited dialog: %+v", got)
				}
				if _, _, ok := s.ProbeToken(got.ProbeTarget(), false); ok || !reflect.DeepEqual(*transitions, []string{"working", "blocked"}) {
					t.Fatal("replacement triggered work", *transitions)
				}
			})
		}
	}
}

func TestProbeRawUDPPriority(t *testing.T) {
	a := probeAgent("blocked")
	applyAttentionClassification(a, questionHit("A"))
	s, _ := probeState(t, a)
	old := s.BeginPoll()
	s.CommitEvent("pane", "idle", 1)
	if _, ok := s.CommitPoll([]*AgentState{a}, nil, old); !ok {
		t.Fatal("poll rejected")
	}
	got, _ := s.Agent("pane")
	if got.Status != "blocked" || got.RawStatus != "idle" || !got.ProbePending {
		t.Fatalf("lost raw precedence: %+v", got)
	}
	inFlight := captureProbe(t, s, false)
	old = s.BeginPoll()
	s.CommitEvent("pane", "working", 2)
	if _, ok := s.CommitPoll([]*AgentState{probeAgent("done")}, nil, old); !ok {
		t.Fatal("poll rejected")
	}
	got, _ = s.Agent("pane")
	if got.Status != "working" || got.RawStatus != "working" || got.ProbePending || s.CommitProbe(inFlight, questionHit("A"), true, false, time.Now()) {
		t.Fatalf("working did not win: %+v", got)
	}
}

func TestProbeHandoffAndDialogIdentity(t *testing.T) {
	for _, hit := range []question.Classification{questionHit("A"), approvalHit("A")} {
		t.Run(string(hit.Kind), func(t *testing.T) {
			a := probeAgent("blocked")
			native := hit
			if hit.Kind == question.AttentionApproval {
				native.ApprovalIdentity = "offered-native"
			}
			applyAttentionClassification(a, native)
			a.LiveIdentity = "A"
			s, transitions := probeState(t, a)
			before, _ := s.Agent("pane")
			s.CommitEvent("pane", "idle", 1)
			now := time.Unix(123, 0)
			if !s.CommitProbe(captureProbe(t, s, false), hit, true, false, now) {
				t.Fatal("not promoted")
			}
			s.CommitEvent("pane", "idle", 2)
			got, _ := s.Agent("pane")
			if got.BlockedEventID != before.BlockedEventID || got.ApprovalFingerprint != before.ApprovalFingerprint {
				t.Fatalf("narrowing rotated event: %+v", got)
			}
			if hit.Kind == question.AttentionQuestion {
				focus := questionHit("A")
				focus.Interaction.Focus.Index = 1
				s.CommitProbe(captureProbe(t, s, false), focus, true, false, now)
				s.CommitProbe(captureProbe(t, s, false), questionHit("B"), true, false, now)
				got, _ = s.Agent("pane")
				if got.BlockedEventID != before.BlockedEventID || got.InteractionID != "B" {
					t.Fatalf("step did not advance: %+v", got)
				}
				a.LiveIdentity = "B"
				applyAttentionClassification(a, questionHit("B"))
			}
			s.CommitInventory([]*AgentState{a}, s.RevisionCounter())
			got, _ = s.Agent("pane")
			if got.RawStatus != "blocked" || got.Status != "blocked" || got.ScreenAttention != "" || got.BlockedEventID != before.BlockedEventID || got.ApprovalFingerprint != before.ApprovalFingerprint || len(*transitions) != 1 {
				t.Fatalf("native handoff changed event: %+v / %v", got, *transitions)
			}
			if hit.Kind == question.AttentionApproval {
				s.CommitEvent("pane", "idle", 3)
				s.CommitProbe(captureProbe(t, s, false), hit, true, false, now)
				s.CommitProbe(captureProbe(t, s, false), approvalHit("B"), true, false, now)
				got, _ = s.Agent("pane")
				if got.BlockedEventID == before.BlockedEventID || got.ApprovalFingerprint != "B" || len(*transitions) != 2 {
					t.Fatalf("new approval not announced: %+v / %v", got, *transitions)
				}
			}
		})
	}
}

func TestProbeRejectedReadFinishesOnlyCurrentStop(t *testing.T) {
	for _, scenario := range []string{"revision", "error", "working", "replacement"} {
		t.Run(scenario, func(t *testing.T) {
			s, transitions := probeState(t, probeAgent("working"))
			s.CommitEvent("pane", "idle", 1)
			token := captureProbe(t, s, false)
			final := "idle"
			switch scenario {
			case "revision":
				a := probeAgent("idle")
				a.PaneRevision++
				s.CommitInventory([]*AgentState{a}, s.RevisionCounter())
			case "working":
				final = "working"
				s.CommitEvent("pane", "working", 2)
			case "replacement":
				a := probeAgent("idle")
				a.TerminalID = "replacement"
				s.CommitTopology([]*AgentState{a}, nil, s.RevisionCounter())
			}
			wantCompletion := scenario == "revision" || scenario == "error"
			if changed := s.CommitProbe(token, questionHit("A"), scenario != "error", false, time.Now()); changed != wantCompletion {
				t.Fatalf("changed=%t", changed)
			}
			got, _ := s.Agent("pane")
			if got.Status != final || got.ProbePending || s.CompletionCurrent("pane", got.StateRevision) != wantCompletion || s.CommitProbe(token, questionHit("A"), true, false, time.Now()) {
				t.Fatalf("bad stop resolution: %+v %v", got, *transitions)
			}
			if wantCompletion && !s.RegisterFinishedNotificationForTransition("pane", got.StateRevision) {
				t.Fatal("completion was not registrable")
			}
		})
	}
}
