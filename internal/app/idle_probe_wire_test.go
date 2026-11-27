package app

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/question"
)

func TestIdleProbeCapabilityOnBothBackends(t *testing.T) {
	s := testServer()
	for _, backend := range []string{"herdr", "hc"} {
		for _, enabled := range []bool{false, true} {
			s.cfg.Backend = backend
			s.cfg.IdleQuestionProbe = enabled
			if got := slices.Contains(s.effectiveCapabilitiesFor(herdr.ServerStatus{}), protocol.IdleQuestionProbeCapability); got != enabled {
				t.Errorf("%s: capability present=%t, want %t", backend, got, enabled)
			}
		}
	}
}

func TestIdleProbePublishedControlsAndClear(t *testing.T) {
	for _, kind := range []question.AttentionKind{question.AttentionQuestion, question.AttentionApproval} {
		t.Run(string(kind), func(t *testing.T) {
			e := newFrameEnv(t)
			s := e.server
			s.state.EnableIdleQuestionProbe()
			s.cfg.IdleQuestionProbe = true
			s.transitionBroadcast = s.hub
			s.state.CommitEvent("pane-1", "idle", 1)
			now := time.Unix(100, 0)
			commit := func(classification question.Classification) bool {
				t.Helper()
				a, _ := s.state.Agent("pane-1")
				token, _, ok := s.state.ProbeToken(a.TargetRef(), false)
				if !ok {
					t.Fatal("missing token")
				}
				return s.state.CommitProbe(token, classification, true, false, now)
			}
			frame := probeQuestion(t)
			if kind == question.AttentionApproval {
				frame = probeApproval(t)
			}
			live, _ := question.ClassifyLive(frame, "claude")
			if !commit(live) {
				t.Fatal("not promoted")
			}
			a, _ := s.state.Agent("pane-1")
			s.broadcastBlockedAttention(a)
			blocked := readFrame(t, e.conn)
			if blocked["type"] != "blocked" || blocked["screen_attention"] != coordinator.ScreenWaiting ||
				blocked["status_since_at"] != float64(now.UnixMilli()) {
				t.Fatal(blocked)
			}
			for _, report := range []string{"idle", "done", "idle"} {
				s.state.CommitEvent("pane-1", report, 2)
				a, _ = s.state.Agent("pane-1")
				s.commitAgent(a, "agent_update")
				updated := readFrame(t, e.conn)
				if updated["screen_attention"] != coordinator.ScreenWaiting || updated["status_since_at"] != float64(now.UnixMilli()) || updated["event_id"] != a.BlockedEventID || updated["status"] != "blocked" || updated["attention_kind"] != string(kind) ||
					kind == question.AttentionQuestion && updated["interaction"] == nil || kind == question.AttentionApproval && updated["options"] == nil {
					t.Fatalf("UDP lost committed controls: %+v", updated)
				}
			}
			if commit(question.Classification{}) {
				t.Fatal("miss changed attention")
			}
			now = now.Add(2 * time.Second)
			if !commit(question.Classification{}) {
				t.Fatal("second miss did not clear")
			}
			publishInventoryForTest(t, s)
			frames := e.settle(t)
			lists := 0
			for _, frame := range frames {
				if frame["type"] != "agents" {
					continue
				}
				lists++
				var agents []*coordinator.AgentState
				decodeInto(t, frame["agents"], &agents)
				if len(agents) != 1 || agents[0].ScreenAttention != coordinator.ScreenCleared || agents[0].Status != "done" || agents[0].StatusSinceAt != now.UnixMilli() || agents[0].Interaction != nil || agents[0].Options != nil || agents[0].BlockedEventID != "" {
					t.Fatal(frame)
				}
			}
			if lists != 1 {
				t.Fatal(frames)
			}
			s.broadcastBlockedAttention(a)
			publishInventoryForTest(t, s)
			if frames := e.settle(t); len(frames) != 0 {
				t.Fatalf("deduplication regressed: %+v", frames)
			}
			if inventory := s.committedInventorySnapshot(); inventory.agents[0].ScreenAttention != coordinator.ScreenCleared {
				t.Fatal(inventory.agents)
			}
		})
	}
}

func TestProbeGovernedPaneFramesRequireMatchingCommittedControls(t *testing.T) {
	for _, status := range []string{"idle", "waiting"} {
		t.Run(status, func(t *testing.T) {
			s, backend, _, _ := newProbeServer(t, true)
			frame := probeQuestion(t)
			agent := &coordinator.AgentState{PaneID: "pane-1", TerminalID: "terminal", Agent: "claude", Status: "idle"}
			s.state.CommitInventory([]*coordinator.AgentState{agent}, 0)
			if status == "waiting" {
				a, _ := s.state.Agent(agent.PaneID)
				token, _, _ := s.state.ProbeToken(a.TargetRef(), true)
				live, _ := question.ClassifyLive(frame, a.Agent)
				if !s.state.CommitProbe(token, live, true, true, time.Now()) {
					t.Fatal("not promoted")
				}
			}
			classify := func(value string) *question.Interaction {
				res := s.classifyPaneResponse(coordinator.PaneReadRequest{PaneID: agent.PaneID, Lines: 100}, map[string]any{"content": value, "viewport_only": false})
				interaction, _ := res["interaction"].(*question.Interaction)
				return interaction
			}
			if got := classify(frame); (got != nil) != (status == "waiting") {
				t.Fatal(got)
			}
			if got := classify(strings.ReplaceAll(frame, "First choice", "Different choice")); got != nil {
				t.Fatal(got)
			}
			if got := classify(frame + "\nThat question is finished"); got != nil {
				t.Fatal(got)
			}
			if len(backend.called("ReadPane")) != 0 {
				t.Fatal("classifying a pane frame made an extra read")
			}
		})
	}
}
