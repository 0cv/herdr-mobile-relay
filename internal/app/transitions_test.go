package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/activity"
	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/push"
	"github.com/0cv/herdr-mobile-relay/internal/question"
)

func TestBothBackendsQueueCurrentWaitingAlerts(t *testing.T) {
	const approval = `
Do you want to proceed?
Bash command
$ true
❯ 1. Yes
  2. Yes, and remember this choice
  3. No
Esc to cancel · Enter to confirm
`
	for _, backend := range []string{"herdr", "hc"} {
		t.Run(backend, func(t *testing.T) {
			path := filepath.Join(testSocketDir(t), "test.sock")
			listener, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			replied := make(chan error, 1)
			go func() {
				replied <- serveWaitingPaneRead(listener, backend, approval)
			}()

			s := handlerTestServer(t)
			s.herdrC = newBackend(&config.Config{Backend: backend, SocketPath: path, HerdrBin: filepath.Join(t.TempDir(), "unused")})
			queued := transitionPushQueue(t, s)
			if !s.state.CommitEvent("pane-1", "blocked", time.Now().UnixMilli()) {
				t.Fatal("could not block pane")
			}
			agent, _ := s.state.Agent("pane-1")
			s.handleTransition(t.Context(), agent.PaneID, agent.Agent, agent.Project, agent.Status, agent.StateRevision, time.Now().UnixMilli())
			select {
			case err := <-replied:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("backend did not read the fixture")
			}
			alerts := queued()
			if len(alerts) != 1 || alerts[0].Key.Category != push.CategoryAttention ||
				alerts[0].Key.EventID != agent.BlockedEventID || !s.pushEventCurrent(alerts[0].Key) {
				t.Fatalf("expected one current approval alert, got %#v", alerts)
			}
			current, _ := s.state.Agent("pane-1")
			if current.AttentionKind != question.AttentionApproval {
				t.Fatalf("pane classified as %q", current.AttentionKind)
			}
			claims, err := s.pushM.VerifyEventReference(alerts[0].EventRef, time.Now().Add(47*time.Hour))
			if err != nil || claims.Key.Target().ServerSessionID != protocol.PrimaryServerSessionID {
				t.Fatalf("late reference = %#v, %v", claims, err)
			}
		})
	}
}

func serveWaitingPaneRead(listener net.Listener, backend, content string) error {
	conn, err := listener.Accept()
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	decoder, encoder := json.NewDecoder(conn), json.NewEncoder(conn)
	reply := func(method string, result map[string]any) error {
		var request struct {
			ID     string `json:"id"`
			Method string `json:"method"`
		}
		if err := decoder.Decode(&request); err != nil {
			return err
		}
		if request.Method != method {
			return fmt.Errorf("backend request = %q, want %q", request.Method, method)
		}
		return encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}
	if backend == "hc" {
		if err := reply("hello", map[string]any{"generation": 1}); err != nil {
			return err
		}
		return reply("terminal.read", map[string]any{"complete": true, "text": content})
	}
	return reply("pane.read", map[string]any{"type": "pane_read", "read": map[string]string{"text": content}})
}

func TestClassifyBlockedTransitionRetriesPartialCodexQuestion(t *testing.T) {
	partial := `
Question 1/3 (3 unanswered)
After a respondent finishes a questionnaire, what should the app do?

› 1. Builder-defined result (Recommended)  Show the configured result.
  2. Response dashboard                    Store responses for review.
  3. Personalized output                   Generate a tailored result.
  4. None of the above                     Optionally, add details in notes (tab).
`
	complete := partial + `
tab to add notes | enter to submit answer | ←/→ to navigate questions | esc to interrupt
`
	calls := 0
	classification, err := classifyBlockedTransition(
		context.Background(),
		"codex",
		func(context.Context) (string, error) {
			calls++
			if calls == 1 {
				return partial, nil
			}
			return complete, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 ||
		classification.Kind != question.AttentionQuestion ||
		classification.Interaction == nil ||
		classification.Interaction.Question !=
			"After a respondent finishes a questionnaire, what should the app do?" {
		t.Fatalf("classification after %d reads = %+v", calls, classification)
	}
}

func transitionPushQueue(t *testing.T, s *Server) func() []push.Payload {
	t.Helper()
	directory := t.TempDir()
	manager, err := push.NewManager(directory, s.logger)
	if err != nil {
		t.Fatal(err)
	}
	subscription := push.Subscription{
		Endpoint: "https://fcm.googleapis.com/phone", DeviceID: "device-a",
	}
	subscription.Keys.P256dh = "key"
	subscription.Keys.Auth = "auth"
	if err := manager.Subscribe(subscription); err != nil {
		t.Fatal(err)
	}
	policy := manager.Policy(subscription.DeviceID, "en")
	policy.Categories[push.CategoryFinished] = true
	if err := manager.SetPolicy(policy); err != nil {
		t.Fatal(err)
	}
	s.pushM = manager
	return func() []push.Payload { return queuedPushPayloads(t, directory) }
}

func queuedPushPayloads(t *testing.T, directory string) []push.Payload {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(directory, "queue.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var queue struct {
		Entries map[string]struct {
			Event push.PushEvent `json:"event"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(data, &queue); err != nil {
		t.Fatal(err)
	}
	payloads := make([]push.Payload, 0, len(queue.Entries))
	for _, entry := range queue.Entries {
		var payload push.Payload
		if err := json.Unmarshal(entry.Event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		payloads = append(payloads, payload)
	}
	return payloads
}

type recordingTransitionBroadcast struct {
	messages chan any
}

func (b *recordingTransitionBroadcast) Broadcast(message any) {
	b.messages <- message
}

func seedAcceptedWaitingAlert(t *testing.T, s *Server, dir string) push.Payload {
	t.Helper()
	subscribeDevice(t, s.pushM, pushIdentity.DeviceID, "https://fcm.googleapis.com/phone")
	target := currentPaneTarget(t, s)
	if !s.state.CommitEvent(target.PaneID, "blocked", time.Now().UnixMilli()) {
		t.Fatal("could not block pane")
	}
	agent, _ := s.state.Agent(target.PaneID)
	if result := publishAttention(t, s, pushIdentity.DeviceID, agent.BlockedEventID, target); result.Queued != 1 {
		t.Fatalf("publish = %#v, want one waiting alert", result)
	}
	payloads := queuedPushPayloads(t, dir)
	if len(payloads) != 1 {
		t.Fatalf("waiting alerts = %#v", payloads)
	}
	alert := payloads[0]
	queuePath := filepath.Join(dir, "queue.json")
	stored, err := json.Marshal(map[string]any{
		"delivered": map[string]any{
			"accepted": map[string]any{
				"key": alert.Key, "tag": alert.Tag, "accepted_at": time.Now(),
				"subscription": s.pushM.Subscriptions()[0],
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(queuePath, stored, 0o600); err != nil {
		t.Fatal(err)
	}
	s.pushM = newPushManager(t, dir)
	if err := s.pushM.Reconcile(t.Context(), s.pushEventCurrent); err != nil {
		t.Fatal(err)
	}
	return alert
}

func TestWorkingTransitionWithdrawsAcceptedAlertAfterUnchangedPoll(t *testing.T) {
	s, conn, client, dir := pushActionServer(t)
	alert := seedAcceptedWaitingAlert(t, s, dir)
	open := func() map[string]any {
		t.Helper()
		s.handlePushOpenRef(client, protocol.Inbound{RequestID: "tap", EventRef: alert.EventRef}, "push_open_ref", pushIdentity)
		return readFrame(t, conn)
	}
	requireCompleted(t, open(), "push_open_ref")

	paneID := alert.Key.PaneID
	var workingRevision int64
	s.state.SetOnTransition(func(_, _, _, status string, revision int64) {
		if status == "working" {
			workingRevision = revision
		}
	})
	if !s.state.CommitEvent(paneID, "working", time.Now().UnixMilli()) || workingRevision == 0 {
		t.Fatal("missing working transition")
	}
	working, _ := s.state.Agent(paneID)
	s.state.CommitInventory([]*coordinator.AgentState{working}, s.state.RevisionCounter())
	if s.state.Revision(paneID) == workingRevision {
		t.Fatal("poll did not advance the pane revision")
	}

	s.handleTransition(t.Context(), paneID, working.Agent, working.Project, working.Status, workingRevision, time.Now().UnixMilli())
	requireFailedResult(t, open(), "push_open_ref", "Notification target is no longer available")
	withdrawals := queuedPushPayloads(t, dir)
	if len(withdrawals) != 1 || !withdrawals[0].Retract || withdrawals[0].Key != alert.Key {
		t.Fatalf("queue = %#v, want one withdrawal of the accepted alert", withdrawals)
	}
	if _, err := s.pushM.VerifyEventReference(alert.EventRef, time.Now()); !errors.Is(err, push.ErrStaleReference) {
		t.Fatalf("withdrawn reference = %v, want stale", err)
	}
}

func TestUnchangedPollPreservesBlockedTransitionSideEffects(t *testing.T) {
	root := t.TempDir()
	s := testServer()
	journal, err := activity.OpenJournal(filepath.Join(root, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	s.dispatcher = coordinator.NewDispatcher(nil, s.state, journal, s.logger)
	t.Cleanup(func() {
		_ = s.dispatcher.Close(context.Background())
	})
	queuedPushes := transitionPushQueue(t, s)
	broadcast := &recordingTransitionBroadcast{messages: make(chan any, 2)}
	s.transitionBroadcast = broadcast
	enrichStarted := make(chan struct{})
	enrichRelease := make(chan struct{})
	s.transitionEnrich = func(_ context.Context, agent *coordinator.AgentState) {
		close(enrichStarted)
		<-enrichRelease
		agent.AttentionKind = "approval"
		agent.Command = "Approve deployment"
		agent.Prompt = "Allow this command?"
		agent.Options = []string{"Approve", "Reject"}
	}
	s.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", TerminalID: "terminal-1", Agent: "codex", Project: "relay", Status: "working",
	}}, s.state.RevisionCounter())
	s.state.CommitEvent("pane-1", "blocked", time.Now().UnixMilli())
	revision := s.state.Revision("pane-1")

	done := make(chan struct{})
	go func() {
		s.handleTransition(context.Background(), "pane-1", "codex", "relay", "blocked", revision, time.Now().UnixMilli())
		close(done)
	}()
	<-enrichStarted
	s.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", TerminalID: "terminal-1", Agent: "codex", Project: "relay", Status: "blocked",
	}}, s.state.RevisionCounter())
	close(enrichRelease)
	<-done

	if entries := journal.Recent(10); len(entries) != 1 ||
		entries[0].Kind != "blocked" || entries[0].Summary != "Approve deployment" {
		t.Fatalf("blocked activity entries = %+v, want exactly one enriched entry", entries)
	}
	payloads := queuedPushes()
	if len(payloads) != 1 {
		t.Fatalf("blocked pushes = %+v, want exactly one", payloads)
	}
	agent, _ := s.state.Agent("pane-1")
	wantKey := push.PushEventKey{
		DeviceID: "device-a", ServerSessionID: "primary", PaneID: agent.PaneID,
		TerminalID: agent.TerminalID, AgentSessionID: agent.SessionID, Generation: agent.Generation,
		EventID: agent.BlockedEventID, InteractionRevision: s.state.AttentionRevision(agent.PaneID),
		Category: push.CategoryAttention,
	}
	if payloads[0].Version != 1 || payloads[0].Key != wantKey || payloads[0].EventRef == "" {
		t.Fatalf("blocked push = %+v, want key %+v with event reference", payloads[0], wantKey)
	}
	if len(broadcast.messages) != 1 {
		t.Fatalf("blocked broadcasts = %d, want exactly one", len(broadcast.messages))
	}
}

func TestBlockedTransitionDoesNotPublishOrBroadcastAfterNewerWorkingState(t *testing.T) {
	s := testServer()
	queuedPushes := transitionPushQueue(t, s)
	broadcast := &recordingTransitionBroadcast{messages: make(chan any, 1)}
	s.transitionBroadcast = broadcast
	enrichStarted := make(chan struct{})
	enrichRelease := make(chan struct{})
	s.transitionEnrich = func(_ context.Context, agent *coordinator.AgentState) {
		close(enrichStarted)
		<-enrichRelease
		agent.AttentionKind = question.AttentionApproval
	}
	s.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", TerminalID: "terminal-1", Agent: "codex", Project: "relay", Status: "working",
	}}, s.state.RevisionCounter())
	s.state.CommitEvent("pane-1", "blocked", time.Now().UnixMilli())
	revision := s.state.Revision("pane-1")

	done := make(chan struct{})
	go func() {
		s.handleTransition(context.Background(), "pane-1", "codex", "relay", "blocked", revision, time.Now().UnixMilli())
		close(done)
	}()
	<-enrichStarted

	s.state.CommitEvent("pane-1", "working", time.Now().UnixMilli())
	close(enrichRelease)
	<-done

	if payloads := queuedPushes(); len(payloads) != 0 {
		t.Fatalf("stale blocked transition queued pushes: %+v", payloads)
	}
	select {
	case message := <-broadcast.messages:
		t.Fatalf("stale blocked transition was broadcast after working revision: %#v", message)
	default:
	}
}

func TestApprovalPushSuppressedWhenAttentionIsReclassified(t *testing.T) {
	s := testServer()
	queuedPushes := transitionPushQueue(t, s)
	broadcast := &recordingTransitionBroadcast{messages: make(chan any, 1)}
	s.transitionBroadcast = broadcast
	enrichStarted := make(chan struct{})
	enrichRelease := make(chan struct{})
	s.transitionEnrich = func(_ context.Context, agent *coordinator.AgentState) {
		close(enrichStarted)
		<-enrichRelease
		agent.AttentionKind = question.AttentionApproval
		agent.Command = "Approve deployment"
		agent.Prompt = "Allow this command?"
		agent.Options = []string{"Approve", "Reject"}
	}
	s.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", TerminalID: "terminal-1", Agent: "codex", Project: "relay", Status: "working",
	}}, s.state.RevisionCounter())
	s.state.CommitEvent("pane-1", "blocked", time.Now().UnixMilli())
	revision := s.state.Revision("pane-1")

	done := make(chan struct{})
	go func() {
		s.handleTransition(context.Background(), "pane-1", "codex", "relay", "blocked", revision, time.Now().UnixMilli())
		close(done)
	}()
	<-enrichStarted

	agent, ok := s.state.Agent("pane-1")
	if !ok {
		t.Fatal("blocked agent disappeared")
	}
	generation, active := s.state.PaneSession("pane-1")
	if !active {
		t.Fatal("blocked pane session disappeared")
	}
	reclassified, _, committed := s.state.CommitAttentionClassification(
		"pane-1",
		agent.BlockedEventID,
		generation,
		s.state.ContentRevision("pane-1"),
		question.Classification{
			Kind:   question.AttentionChat,
			Prompt: "What would you like to work on next?",
		}, "",
	)
	if !committed {
		t.Fatal("chat reclassification was not committed")
	}
	close(enrichRelease)
	<-done

	if payloads := queuedPushes(); len(payloads) != 0 {
		t.Fatalf("reclassified approval queued pushes: %+v", payloads)
	}
	select {
	case message := <-broadcast.messages:
		t.Fatalf("stale approval was broadcast after reclassification: %#v", message)
	default:
	}

	s.handleChatCompletion(context.Background(), reclassified, s.state.CompletionRevision(reclassified.PaneID))
	if payloads := queuedPushes(); len(payloads) != 1 || payloads[0].Key.Category != push.CategoryFinished {
		t.Fatalf("chat reclassification pushes = %+v, want one completion", payloads)
	}
}

func TestChatCompletionWithdrawsAcceptedAlertAfterUnchangedPoll(t *testing.T) {
	s, _, _, dir := pushActionServer(t)
	s.state.EnableIdleQuestionProbe()
	alert := seedAcceptedWaitingAlert(t, s, dir)
	paneID := alert.Key.PaneID
	blockedRevision := s.state.Revision(paneID)

	classified, _, ok := s.state.CommitAttentionClassification(
		paneID, alert.Key.EventID, alert.Key.Generation, s.state.ContentRevision(paneID),
		question.Classification{Kind: question.AttentionChat, Prompt: "The request has been answered."}, "",
	)
	if !ok {
		t.Fatal("could not classify chat completion")
	}
	s.state.CommitInventory([]*coordinator.AgentState{classified}, s.state.RevisionCounter())
	if s.state.Revision(paneID) == classified.StateRevision {
		t.Fatal("poll did not advance the pane revision")
	}
	s.transitionEnrich = func(context.Context, *coordinator.AgentState) {}

	s.handleTransition(t.Context(), paneID, classified.Agent, classified.Project, "blocked", blockedRevision, time.Now().UnixMilli())

	withdrawals := queuedPushPayloads(t, dir)
	if len(withdrawals) != 1 || !withdrawals[0].Retract || withdrawals[0].Key != alert.Key {
		t.Fatalf("queue = %#v, want one withdrawal of the accepted alert", withdrawals)
	}
}

func TestChatClassificationUsesOneCompletionPath(t *testing.T) {
	root := t.TempDir()
	s := testServer()
	journal, err := activity.OpenJournal(filepath.Join(root, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	s.dispatcher = coordinator.NewDispatcher(nil, s.state, journal, s.logger)
	t.Cleanup(func() {
		_ = s.dispatcher.Close(context.Background())
	})
	queuedPushes := transitionPushQueue(t, s)
	broadcast := &recordingTransitionBroadcast{messages: make(chan any, 3)}
	s.transitionBroadcast = broadcast
	s.transitionEnrich = func(_ context.Context, agent *coordinator.AgentState) {
		agent.AttentionKind = "chat"
		agent.Prompt = "Hello! What would you like to work on next?"
		agent.Options = []string{"fabricated", "controls"}
	}
	s.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", TerminalID: "terminal-1", Agent: "codex", Project: "relay", Status: "working",
	}}, s.state.RevisionCounter())
	s.state.CommitEvent("pane-1", "blocked", time.Now().UnixMilli())
	s.handleTransition(
		context.Background(), "pane-1", "codex", "relay", "blocked",
		s.state.Revision("pane-1"), time.Now().UnixMilli(),
	)

	entries := journal.Recent(10)
	if len(entries) != 1 || entries[0].Kind != "finished" ||
		entries[0].Extract != "Hello! What would you like to work on next?" {
		t.Fatalf("chat activities = %+v, want one completion", entries)
	}
	payloads := queuedPushes()
	if len(payloads) != 1 {
		t.Fatalf("chat pushes = %+v, want one completion push", payloads)
	}
	payload := payloads[0]
	if payload.Key.Category != push.CategoryFinished || !strings.Contains(payload.Title, "finished") ||
		len(payload.Actions) != 0 || len(payload.ActionRefs) != 0 {
		t.Fatalf("chat push = %+v", payload)
	}
	message := (<-broadcast.messages).(map[string]any)
	if fmt.Sprint(message["attention_kind"]) != "chat" {
		t.Fatalf("chat broadcast = %+v", message)
	}
	if options, ok := message["options"].([]string); ok && len(options) != 0 {
		t.Fatalf("chat broadcast retained controls: %+v", message)
	}

	s.state.CommitEvent("pane-1", "idle", time.Now().UnixMilli())
	s.handleTransition(
		context.Background(), "pane-1", "codex", "relay", "idle",
		s.state.Revision("pane-1"), time.Now().UnixMilli(),
	)
	if len(journal.Recent(10)) != 1 || len(queuedPushes()) != 1 {
		t.Fatal("raw idle duplicated the classified chat completion")
	}
}

func TestUnknownClassificationHasNoNotificationActions(t *testing.T) {
	root := t.TempDir()
	s := testServer()
	journal, err := activity.OpenJournal(filepath.Join(root, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	s.dispatcher = coordinator.NewDispatcher(nil, s.state, journal, s.logger)
	t.Cleanup(func() {
		_ = s.dispatcher.Close(context.Background())
	})
	queuedPushes := transitionPushQueue(t, s)
	s.transitionBroadcast = &recordingTransitionBroadcast{messages: make(chan any, 1)}
	s.transitionEnrich = func(_ context.Context, agent *coordinator.AgentState) {
		agent.AttentionKind = "unknown"
		agent.Prompt = "Agent needs inspection"
		agent.Options = []string{"Approve", "Reject"}
	}
	s.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", TerminalID: "terminal-1", Agent: "opencode", Project: "relay", Status: "working",
	}}, s.state.RevisionCounter())
	s.state.CommitEvent("pane-1", "blocked", time.Now().UnixMilli())
	s.handleTransition(
		context.Background(), "pane-1", "opencode", "relay", "blocked",
		s.state.Revision("pane-1"), time.Now().UnixMilli(),
	)

	payloads := queuedPushes()
	if len(payloads) != 1 {
		t.Fatalf("unknown pushes = %+v, want one inspection push", payloads)
	}
	payload := payloads[0]
	if payload.Key.Category != push.CategoryAttention || payload.Title != "Herdr notification" ||
		len(payload.Actions) != 0 || len(payload.ActionRefs) != 0 {
		t.Fatalf("unknown push = %+v", payload)
	}
	agent, _ := s.state.Agent("pane-1")
	if len(agent.Options) != 0 || agent.Interaction != nil {
		t.Fatalf("unknown classification retained controls: %+v", agent)
	}
}
