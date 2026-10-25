package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/activity"
	"github.com/0cv/herdr-mobile-relay/internal/conversation"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

type kimiProcessFixture func(context.Context, string) (*herdr.PaneProcessInfo, error)

func (f kimiProcessFixture) PaneProcessInfoFresh(ctx context.Context, pane string) (*herdr.PaneProcessInfo, error) {
	return f(ctx, pane)
}

func TestKimiE2dProcessIncarnation(t *testing.T) {
	reader := kimiProcessFixture(func(context.Context, string) (*herdr.PaneProcessInfo, error) {
		return &herdr.PaneProcessInfo{PaneID: "pane", ShellPID: 10, ForegroundProcessGroupID: 20, ForegroundProcesses: []herdr.PaneProcess{{PID: 20, Cwd: "/fixture", Argv: []string{"kimi"}}}}, nil
	})
	identity := func(start string) string {
		t.Helper()
		value, _, err := kimiVerifiedProcess(t.Context(), reader, "pane", func(int) (string, error) { return start, nil })
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	first, replacement := identity("100:000001"), identity("100:000002")
	if first == replacement || !strings.HasPrefix(first, "verified:") {
		t.Fatal("equal PID/CWD/argv hid process replacement")
	}
	calls := 0
	if _, _, err := kimiVerifiedProcess(t.Context(), reader, "pane", func(int) (string, error) {
		calls++
		return fmt.Sprint(calls), nil
	}); err == nil {
		t.Fatal("replacement during verification accepted")
	}
	server, _, wire := kimiIdentityServer(t)
	owner := func(process string) *coordinator.AgentState {
		agent := &coordinator.AgentState{PaneID: "pane", Agent: "kimi", Cwd: "/fixture", ForegroundCwd: "/fixture", ConversationProcess: process}
		server.resolveKimiSessions([]*coordinator.AgentState{agent})
		return agent
	}
	before, after := owner(first), owner(replacement)
	if before.ConversationInferredOwner == "" || before.ConversationInferredOwner == after.ConversationInferredOwner {
		t.Fatal("replacement retained inferred owner")
	}
	if owner(identity("")).ConversationInferredOwner != "" {
		t.Fatal("unavailable incarnation claimed owner continuity")
	}
	unavailable, _, err := kimiVerifiedProcess(t.Context(), reader, "pane", func(int) (string, error) { return "", errors.New("unavailable") })
	if err != nil || owner(unavailable).ConversationInferredOwner != "" {
		t.Fatal("lookup failure claimed owner continuity")
	}
	changeKimiIdentity(t, server, before, wire, "switch")
	nextSession := owner(first)
	if nextSession.Session == before.Session || nextSession.ConversationInferredOwner != before.ConversationInferredOwner {
		t.Fatal("same established process lost session continuity")
	}
	server.startedAt = server.startedAt.Add(time.Nanosecond)
	if owner(first).ConversationInferredOwner == before.ConversationInferredOwner {
		t.Fatal("relay replacement retained inferred owner")
	}
}

func TestKimiE2dInitialPeersAndRecovery(t *testing.T) {
	for _, cwd := range []string{"", "/elsewhere"} {
		t.Run("fallback="+cwd, func(t *testing.T) {
			server, _, _ := kimiIdentityServer(t)
			for _, pane := range []string{"second_failure_empty", "second_stale_elsewhere", "second_departed"} {
				agent := &coordinator.AgentState{PaneID: "pane", Agent: "kimi", Cwd: "/fixture"}
				peer := &coordinator.AgentState{PaneID: pane, Agent: "kimi", Cwd: cwd, ForegroundCwd: cwd, ConversationProcess: "cached-identity"}
				server.enrichKimiSessions(t.Context(), []*coordinator.AgentState{agent, peer})
				server.resolveAgentSessionName(agent)
				if pane == "second_departed" {
					if agent.SessionID != "session_old" {
						t.Fatalf("fresh departure prevented recovery: %+v", agent)
					}
				} else if agent.ConversationReason != "ambiguous_session" || agent.SessionID != "" {
					t.Fatalf("unverified or changed-into-target peer ignored: %+v", agent)
				}
			}
		})
	}
}

func TestKimiE2dPeerCapAndDeadline(t *testing.T) {
	server, agent, _ := kimiIdentityServer(t)
	peers := []*coordinator.AgentState{agent}
	for i := range maxKimiProcessChecks {
		peers = append(peers, &coordinator.AgentState{PaneID: fmt.Sprintf("second_departed_%d", i), Agent: "kimi", Cwd: "/elsewhere"})
	}
	server.state.CommitInventory(peers, server.state.RevisionCounter())
	if !server.kimiSelectionCurrent(t.Context(), agent) {
		t.Fatal("32 freshly different peers should be ruled out")
	}
	peers = append(peers, &coordinator.AgentState{PaneID: "second_departed_over_cap", Agent: "kimi", Cwd: "/elsewhere"})
	server.state.CommitInventory(peers, server.state.RevisionCounter())
	if server.kimiSelectionCurrent(t.Context(), agent) {
		t.Fatal("peer cap did not fail closed")
	}
	initial := make([]*coordinator.AgentState, len(peers))
	for i, peer := range peers {
		initial[i] = &coordinator.AgentState{PaneID: peer.PaneID, Agent: "kimi", Cwd: peer.Cwd}
	}
	server.enrichKimiSessions(t.Context(), initial)
	if initial[0].ConversationReason != "ambiguous_session" || initial[len(initial)-1].ConversationProcess != "" {
		t.Fatal("initial cap reused cached evidence")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	at := time.Now()
	if server.kimiSelectionCurrent(ctx, agent) || time.Since(at) > 200*time.Millisecond {
		t.Fatal("expired shared deadline accepted or blocked")
	}
}

func TestKimiE2dUnavailableRecovery(t *testing.T) {
	server, _, _ := kimiIdentityServer(t)
	unavailable := &coordinator.AgentState{PaneID: "second_failure", Agent: "kimi", Cwd: "/fixture", ConversationInferred: true, ConversationReason: "source_unavailable"}
	server.state.CommitInventory([]*coordinator.AgentState{unavailable}, server.state.RevisionCounter())
	if !server.kimiSelectionCurrent(t.Context(), unavailable) {
		t.Fatal("stable failed lookup caused a reset")
	}
	unavailable.PaneID = "pane"
	server.state.CommitInventory([]*coordinator.AgentState{unavailable}, server.state.RevisionCounter())
	if server.kimiSelectionCurrent(t.Context(), unavailable) {
		t.Fatal("fresh process evidence did not wake unavailable recovery")
	}
	unavailable.ConversationReason = "ambiguous_session"
	peers := []*coordinator.AgentState{unavailable}
	for i := range maxKimiProcessChecks + 1 {
		peers = append(peers, &coordinator.AgentState{PaneID: fmt.Sprintf("second_departed_%d", i), Agent: "kimi", Cwd: "/elsewhere"})
	}
	server.state.CommitInventory(peers, server.state.RevisionCounter())
	for range 3 {
		if !server.kimiSelectionCurrent(t.Context(), unavailable) {
			t.Fatal("stable capped unavailable selection caused a reset")
		}
	}
	server.state.CommitInventory([]*coordinator.AgentState{unavailable}, server.state.RevisionCounter())
	if server.kimiSelectionCurrent(t.Context(), unavailable) {
		t.Fatal("topology removal did not wake capped recovery")
	}
}

func kimiE2dDuplicateSource(t *testing.T) (*Server, *coordinator.AgentState, string) {
	t.Helper()
	server, _, first := kimiIdentityServer(t)
	firstDir := filepath.Dir(filepath.Dir(filepath.Dir(first)))
	firstRoot := filepath.Dir(filepath.Dir(filepath.Dir(firstDir)))
	home := t.TempDir()
	second := appKimiFixture(t, home, "session_old")
	data, err := os.ReadFile(second)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte(strings.ReplaceAll(string(data), "Seed answer", "SELECTED_ROOT_ANSWER")), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(firstDir, "state.json"), []byte(`{"id":"session_old","version":2,"cwd":"/fixture","updatedAt":1000,"archived":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_KIMI_CODE_HOME_DIRS", firstRoot)
	t.Setenv("KIMI_CODE_HOME", "")
	server.conversationB.Close()
	server.conversationM = conversation.NewReader(home)
	browser, err := conversation.NewBrowser(server.conversationM, t.TempDir(), conversation.DefaultBrowserOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { browser.Close() })
	server.conversationB = browser
	agent := &coordinator.AgentState{PaneID: "pane", TerminalID: "terminal", ServerSessionID: "primary", Agent: "kimi", Cwd: "/fixture", Status: "working"}
	server.enrichKimiSessions(t.Context(), []*coordinator.AgentState{agent})
	server.resolveAgentSessionName(agent)
	server.state.CommitInventory([]*coordinator.AgentState{agent}, server.state.RevisionCounter())
	agent, _ = server.state.Agent("pane")
	return server, agent, second
}

func TestKimiE2dReportedContentPolicy(t *testing.T) {
	server, agent, _ := kimiE2dDuplicateSource(t)
	agent.ConversationInferred, agent.ConversationSource, agent.ConversationInferredOwner = false, "", ""
	server.state.CommitInventory([]*coordinator.AgentState{agent}, server.state.RevisionCounter())
	agent, _ = server.state.Agent(agent.PaneID)
	if got := server.captureFinishedPane(t.Context(), agent.PaneID, agent.Agent, agent.Cwd, agent.SessionID); got != "Seed answer" {
		t.Fatalf("reported-ID completion changed: %q", got)
	}
	entries := server.enrichActivityResponses([]activity.Entry{{Kind: "finished", PaneID: agent.PaneID, Agent: "kimi", Session: agent.SessionID}})
	if entries[0].Extract != "Seed answer" {
		t.Fatal("reported-ID activity changed")
	}
	server.state.CommitInventory(nil, server.state.RevisionCounter())
	entries = server.enrichActivityResponses([]activity.Entry{{Kind: "finished", PaneID: agent.PaneID, Agent: "kimi", Session: agent.SessionID, Extract: "captured extract"}})
	if entries[0].Extract != "captured extract" {
		t.Fatal("ownerless activity was re-resolved by ID")
	}
}

func TestKimiE2dContentCallers(t *testing.T) {
	for _, caller := range []string{"completion", "activity"} {
		t.Run(caller, func(t *testing.T) {
			server, agent, wire := kimiE2dDuplicateSource(t)
			read := func() string {
				if caller == "completion" {
					return server.captureFinishedPane(t.Context(), agent.PaneID, agent.Agent, agent.Cwd, agent.SessionID, projectContextForAgent(agent))
				}
				entries := server.enrichActivityResponses([]activity.Entry{{Kind: "finished", PaneID: agent.PaneID, Agent: "kimi", Session: agent.SessionID}})
				return entries[0].Extract
			}
			if got := read(); got != "SELECTED_ROOT_ANSWER" {
				t.Fatalf("read archived duplicate instead of selected root: %q", got)
			}
			server.conversationHistoryReadObserver = func() {
				changed := *agent
				changed.ConversationSource = "obsolete-source"
				server.state.CommitInventory([]*coordinator.AgentState{&changed}, server.state.RevisionCounter())
			}
			if got := read(); got != "" {
				t.Fatalf("obsolete asynchronous selection exported: %q", got)
			}
			server.conversationHistoryReadObserver = nil
			server.state.CommitInventory([]*coordinator.AgentState{agent}, server.state.RevisionCounter())
			agent, _ = server.state.Agent(agent.PaneID)
			server.conversationHistoryReadObserver = func() {
				if err := os.Remove(wire); err != nil {
					t.Fatal(err)
				}
			}
			if got := read(); got != "" {
				t.Fatalf("root removal during read exported: %q", got)
			}
			server.conversationHistoryReadObserver = nil
			if got := read(); got != "" {
				t.Fatalf("read failure fell back to archived duplicate: %q", got)
			}
		})
	}
}
