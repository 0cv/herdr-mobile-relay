package coordinator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/question"
	"github.com/0cv/herdr-mobile-relay/internal/textutil"
)

func liveFixture(t *testing.T, set, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "question", "testdata", set, name+".ansi"))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func splitHint(frame, hint, wrapped string) string {
	rows := strings.Split(frame, "\n")
	for i, row := range rows {
		if strings.TrimSpace(textutil.StripANSI(row)) == hint {
			rows[i] = wrapped
			break
		}
	}
	return strings.Join(rows, "\n")
}

func narrowQuestion(t *testing.T, name string) string {
	return splitHint(liveFixture(t, "attention", name), "Enter to select · ↑/↓ to navigate · Esc to cancel", "Enter to select · ↑/↓ to navigate · Esc to\ncancel")
}

func narrowApproval(t *testing.T) string {
	return splitHint(liveFixture(t, "attention", "claude-single-approval"), "Esc to cancel · Tab to amend", "Esc to cancel · Tab to\namend")
}

func promoteFrame(t *testing.T, s *State, frame, agent string) *AgentState {
	t.Helper()
	c, ok := question.ClassifyLive(frame, agent)
	if !ok || !s.CommitProbe(captureProbe(t, s, false), c, true, false, time.Now()) {
		t.Fatalf("frame did not promote (%+v)", c)
	}
	got, _ := s.Agent("pane")
	return got
}

func newPromotedDispatcher(t *testing.T, frame, agent string) (*Dispatcher, *fakeBackend, *AgentState) {
	t.Helper()
	a := probeAgent("working")
	a.Agent = agent
	s, _ := probeState(t, a)
	s.CommitEvent("pane", "idle", 1)
	client := newFakeBackend()
	client.contents["pane"] = frame
	d := newTestDispatcher(t, client, s)
	return d, client, promoteFrame(t, s, frame, agent)
}

func probeResponse(a *AgentState, index int) protocol.Inbound {
	if a.AttentionKind == question.AttentionQuestion {
		return protocol.Inbound{Type: "answer_question", RequestID: "question", PaneID: "pane", InteractionID: a.InteractionID, SelectedIndices: []int{index}}
	}
	return protocol.Inbound{Type: "respond", RequestID: a.BlockedEventID, PaneID: "pane", EventID: a.BlockedEventID,
		ApprovalFingerprint: a.ApprovalFingerprint, Choice: a.Options[index], Index: intPtr(index), Total: intPtr(len(a.Options))}
}

func TestProbeNarrowQuestionAndApprovalInput(t *testing.T) {
	for name, frame := range map[string]string{"question": narrowQuestion(t, "claude-single-question"), "approval": narrowApproval(t)} {
		t.Run(name, func(t *testing.T) {
			d, client, a := newPromotedDispatcher(t, frame, "claude")
			if result := d.Handle(t.Context(), probeResponse(a, 1)); !result.OK {
				t.Fatalf("%+v", result)
			}
			var keys []string
			for _, call := range client.callsTo("SendKeys") {
				keys = append(keys, call.Args[1].([]string)...)
			}
			if !reflect.DeepEqual(keys, []string{"Down", "Enter"}) || client.count("ReadPane") != 0 || client.count("ReadPaneVisible") != 1 {
				t.Fatal(client.recorded())
			}
		})
	}
}

func TestProbeRejectsStaleOrReplacedInput(t *testing.T) {
	for _, tc := range []struct{ name, agent, fixture, stale string }{
		{"OMP ask", "omp", "omp-ask-status-below", "omp-ask-stale-before-status"},
		{"Pi ask", "pi", "omp-ask-status-below", "omp-ask-stale-before-status"},
		{"OMP approval", "omp", "omp-tool-approval", "omp-tool-approval-stale-before-status"},
		{"Pi approval", "pi", "omp-tool-approval", "omp-tool-approval-stale-before-status"},
		{"Done after footer", "claude", "claude-single-approval", ""},
		{"read error", "claude", "claude-single-question", ""},
		{"generation change", "claude", "claude-single-question", ""},
		{"confirmed end to done", "claude", "claude-single-question", ""},
		{"changed interaction", "claude", "claude-single-question", ""},
		{"wrong option", "claude", "claude-single-approval", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frame := liveFixture(t, "attention", tc.fixture)
			d, client, a := newPromotedDispatcher(t, frame, tc.agent)
			msg := probeResponse(a, 0)
			if tc.stale != "" {
				client.contents["pane"] = liveFixture(t, "live", tc.stale)
			}
			now := time.Now()
			switch tc.name {
			case "Done after footer":
				client.contents["pane"] = frame + "\nDone"
			case "wrong option":
				msg.Choice = "Other"
			case "changed interaction":
				client.contents["pane"] = narrowQuestion(t, "claude-named-session-task-panel")
			}
			client.setHook("ReadPaneVisible", func(context.Context, fakeCall) error {
				switch tc.name {
				case "read error":
					return errors.New("read failed")
				case "generation change":
					d.state.BumpGeneration("pane")
				case "confirmed end to done":
					d.state.CommitEvent("pane", "done", 2)
					d.state.CommitProbe(captureProbe(t, d.state, false), question.Classification{}, true, false, now)
					d.state.CommitProbe(captureProbe(t, d.state, false), question.Classification{}, true, false, now.Add(2*time.Second))
				}
				return nil
			})
			if result := d.Handle(t.Context(), msg); result.OK || client.count("SendKeys") != 0 || client.count("SendText") != 0 {
				t.Fatalf("unexpected input: %+v %+v", result, client.recorded())
			}
		})
	}
}

func TestProbePendingNativeStopCannotAnswer(t *testing.T) {
	frame := narrowQuestion(t, "claude-single-question")
	c, _ := question.ClassifyLive(frame, "claude")
	a := probeAgent("blocked")
	applyAttentionClassification(a, c)
	s, _ := probeState(t, a)
	s.CommitEvent("pane", "idle", 1)
	client := newFakeBackend()
	client.contents["pane"] = frame
	d := newTestDispatcher(t, client, s)
	if result := d.Handle(t.Context(), probeResponse(a, 0)); result.OK || len(client.recorded()) != 0 {
		t.Fatalf("unexpected input: %+v %+v", result, client.recorded())
	}
}

func TestSequentialPromotedApprovalsUseIndependentEvents(t *testing.T) {
	frame := narrowApproval(t)
	d, client, a := newPromotedDispatcher(t, frame, "claude")
	answerA := probeResponse(a, 0)
	if result := d.Handle(t.Context(), answerA); !result.OK {
		t.Fatalf("A: %+v", result)
	}
	frameB := strings.ReplaceAll(frame, "notes.md", "other.md")
	b := promoteFrame(t, d.state, frameB, "claude")
	client.contents["pane"] = frameB
	answerB := probeResponse(b, 0)
	if result := d.Handle(t.Context(), answerB); !result.OK {
		t.Fatalf("B: %+v", result)
	}
	if result := d.Handle(t.Context(), answerB); !result.OK {
		t.Fatalf("B replay: %+v", result)
	}
	if result := d.Handle(t.Context(), answerA); result.OK || client.count("SendKeys") != 2 || b.BlockedEventID == a.BlockedEventID {
		t.Fatalf("A replay: %+v / %v", result, client.recorded())
	}
}

func TestPromotedQuestionWatcherUsesStrictVisibleScreen(t *testing.T) {
	first := narrowQuestion(t, "claude-plan-multi-question")
	first = splitHint(first, "Enter to select · Tab/Arrow keys to navigate · Esc to cancel", "Enter to select · Tab/Arrow keys to\nnavigate · Esc to cancel")
	first = strings.ReplaceAll(first, "☒ Database", "☐ Database")
	frame := narrowQuestion(t, "claude-plan-submit-answers")
	d, client, a := newPromotedDispatcher(t, first, "claude")
	results := make(chan map[string]any, 8)
	d.SetBroadcast(func(message any) {
		if result, ok := message.(map[string]any); ok {
			results <- result
		}
	})
	if result := d.Handle(t.Context(), probeResponse(a, a.Interaction.Focus.Index)); !result.OK {
		t.Fatalf("%+v", result)
	}
	client.mu.Lock()
	client.contents["pane"] = frame
	client.mu.Unlock()
	select {
	case result := <-results:
		if result["phase"] != "advanced" || client.count("ReadPane") != 0 {
			t.Fatalf("result=%+v calls=%+v", result, client.recorded())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no watcher result")
	}
}
