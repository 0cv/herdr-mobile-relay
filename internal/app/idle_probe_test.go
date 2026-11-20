package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/activity"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/push"
	"github.com/0cv/herdr-mobile-relay/internal/question"
	"github.com/0cv/herdr-mobile-relay/internal/textutil"
)

type probeBackend struct {
	frameBackend
	inventory    []herdr.Pane
	inventoryErr error
	events       chan herdr.TopologySnapshot
	visible      func(context.Context, string) error
	history      func(context.Context) error
}

func (b *probeBackend) GetInventory(context.Context) (*herdr.Inventory, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return &herdr.Inventory{Panes: slices.Clone(b.inventory)}, b.inventoryErr
}

func (b *probeBackend) TabList(context.Context) ([]herdr.Tab, error) { return nil, nil }

func (b *probeBackend) Events(context.Context) (herdr.TopologySnapshot, herdr.TopologyStream, error) {
	return herdr.TopologySnapshot{}, probeEventStream{b.events}, nil
}

type probeEventStream struct{ events <-chan herdr.TopologySnapshot }

func (e probeEventStream) Next(ctx context.Context) (herdr.TopologySnapshot, error) {
	select {
	case snapshot := <-e.events:
		return snapshot, nil
	case <-ctx.Done():
		return herdr.TopologySnapshot{}, ctx.Err()
	}
}

func (probeEventStream) Close() error { return nil }

func (b *probeBackend) ReadPaneVisible(ctx context.Context, paneID string, lines int, format string) (herdr.PaneRead, error) {
	read, err := b.frameBackend.ReadPaneVisible(ctx, paneID, lines, format)
	if b.visible != nil {
		err = b.visible(ctx, paneID)
	}
	return read, err
}

func (b *probeBackend) ReadPane(ctx context.Context, paneID string, lines int, format string) (herdr.PaneRead, error) {
	read, err := b.frameBackend.ReadPane(ctx, paneID, lines, format)
	if b.history != nil {
		err = b.history(ctx)
	}
	return read, err
}

func (b *probeBackend) setInventory(panes ...herdr.Pane) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.inventory = panes
}

func (b *probeBackend) setScreen(content string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.content = content
}

type probeLeases struct {
	paneSizer
	mu     sync.Mutex
	byPane map[string]string
}

func (l *probeLeases) Leased(paneID, terminalID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return terminalID != "" && l.byPane[paneID] == terminalID
}

func (l *probeLeases) put(paneID, terminalID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.byPane[paneID] = terminalID
}

func newProbeServer(t *testing.T, enabled bool) (*Server, *probeBackend, *probeLeases, *idleProbeWorker) {
	t.Helper()
	s := testServerWithCacheDir(t.TempDir())
	b := &probeBackend{events: make(chan herdr.TopologySnapshot)}
	l := &probeLeases{byPane: make(map[string]string)}
	s.herdrC, s.paneSizeM = b, l
	s.cfg.IdleQuestionProbe = enabled
	s.poller = coordinator.NewPoller(b, s.state, time.Hour, s.logger)
	return s, b, l, s.configureIdleProbe()
}

func waitProbe[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case result := <-ch:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("probe barrier timed out")
	}
	var zero T
	return zero
}

func probeQuestion(t *testing.T) string {
	return probeFixture(t, "claude-named-session-task-panel", "Enter to select · ↑/↓ to navigate · Esc to cancel", "Enter to select · ↑/↓ to navigate · Esc to\ncancel")
}

func probeApproval(t *testing.T) string {
	return probeFixture(t, "claude-single-approval", "Esc to cancel · Tab to amend", "Esc to cancel · Tab to\namend")
}

func probeFixture(t *testing.T, name, hint, wrapped string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "question", "testdata", "attention", name+".ansi"))
	if err != nil {
		t.Fatal(err)
	}
	rows := strings.Split(string(data), "\n")
	for i, row := range rows {
		if strings.TrimSpace(textutil.StripANSI(row)) == hint {
			rows[i] = wrapped
			return strings.Join(rows, "\n")
		}
	}
	t.Fatal("fixture hint not found")
	return ""
}

func TestIdleProbeBudgetAcrossStops(t *testing.T) {
	s, backend, _, probe := newProbeServer(t, true)
	backend.setScreen("Work complete")
	var agents []*coordinator.AgentState
	for i := range 10 {
		agents = append(agents, &coordinator.AgentState{PaneID: fmt.Sprintf("pane-%02d", i), TerminalID: fmt.Sprint(i), Agent: "claude", Status: "working"})
	}
	s.state.CommitInventory(agents, 0)
	var completed []string
	s.state.SetOnTransition(func(paneID, _, _, status string, _ int64) {
		if status == "idle" || status == "done" {
			completed = append(completed, paneID)
		}
	})
	for _, a := range agents {
		s.state.CommitEvent(a.PaneID, "idle", 1)
		probe.offer(a.PaneID)
	}
	if probe.readNext(t.Context()) {
		t.Fatal("startup credits must be empty")
	}
	for i, total := range []int{4, 8, 10} {
		probe.reconcile(true)
		for probe.readNext(t.Context()) {
			for _, a := range agents {
				s.state.CommitEvent(a.PaneID, "done", 2)
				probe.offer(a.PaneID)
			}
			probe.refresh()
		}
		if got := len(backend.called("ReadPaneVisible")); got != total || len(completed) != total {
			t.Fatalf("cycle %d: %d reads, %d completions", i, got, len(completed))
		}
	}
	for i, id := range completed {
		if id != agents[i].PaneID {
			t.Fatal(completed)
		}
	}
	if len(backend.called("ReadPane ")) != 0 {
		t.Fatal("unexpected scrollback read")
	}
}

func TestIdleProbeRotationWithRolloverAndRelease(t *testing.T) {
	s, backend, leases, probe := newProbeServer(t, true)
	backend.setScreen(probeQuestion(t))
	var agents []*coordinator.AgentState
	for i := range 7 {
		status := "idle"
		if i < 4 || i == 6 {
			status = "working"
		}
		agents = append(agents, &coordinator.AgentState{PaneID: fmt.Sprintf("p%d", i), TerminalID: fmt.Sprint(i), Agent: "claude", Status: status})
	}
	s.state.CommitInventory(agents, 0)
	for _, a := range agents[:4] {
		s.state.CommitEvent(a.PaneID, "idle", 1)
	}
	probe.reconcile(true)
	for probe.readNext(t.Context()) {
	}
	entered, release := make(chan string), make(chan struct{})
	backend.visible = func(ctx context.Context, paneID string) error {
		select {
		case entered <- paneID:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); probe.run(ctx) }()
	probe.reconcile(true)
	if got := waitProbe(t, entered); got != "p0" {
		t.Fatal(got)
	}
	leases.put("p4", "4")
	leases.put("p5", "5")
	s.state.CommitEvent("p6", "idle", 2)
	probe.refresh()
	probe.offer("p6")
	probe.reconcile(true)
	leases.put("p4", "")
	if got := len(backend.called("ReadPaneVisible")); got != 5 {
		t.Fatalf("rollover started a concurrent read: %d", got)
	}
	close(release)
	for _, want := range []string{"p1", "p2", "p3", "p0"} {
		if got := waitProbe(t, entered); got != want {
			t.Fatalf("rotation got %s want %s", got, want)
		}
	}
	probe.reconcile(true)
	if got := waitProbe(t, entered); got != "p5" {
		t.Fatalf("leased pane starved: %s", got)
	}
	if got := waitProbe(t, entered); got != "p6" {
		t.Fatalf("new stop starved: %s", got)
	}
	cancel()
	waitProbe(t, done)
	for _, read := range backend.called("ReadPaneVisible") {
		if strings.Contains(read, " p4 ") {
			t.Fatal("released pane read")
		}
	}
}

func TestIdleProbePollTopologyAndUDPUseCommittedState(t *testing.T) {
	s, backend, leases, probe := newProbeServer(t, true)
	ctx, cancel := context.WithCancel(t.Context())
	var threads sync.WaitGroup
	defer func() { cancel(); threads.Wait() }()
	published := make(chan struct{}, 8)
	s.poller.SetOnInventoryChange(func() error {
		err := s.publishCurrentInventory(ctx)
		published <- struct{}{}
		return err
	})
	run := func(fn func(context.Context)) { threads.Go(func() { fn(ctx) }) }
	p := herdr.Pane{ID: "pane-1", TerminalID: "terminal", Agent: "claude", Status: "idle"}
	backend.setInventory()
	run(s.poller.Run)
	waitProbe(t, published)
	if probe.readNext(ctx) {
		t.Fatal("startup swept empty inventory")
	}
	leases.put(p.ID, p.TerminalID)
	backend.setInventory(p)
	run(s.poller.RunEvents)
	waitProbe(t, published)
	now := time.Unix(123, 0)
	probe.now = func() time.Time { return now }
	backend.setScreen(probeQuestion(t))
	if !probe.readNext(ctx) {
		t.Fatal("topology did not discover lease using existing credits")
	}
	check := func(status, marker, first string) {
		t.Helper()
		view := s.committedInventorySnapshot().agents
		if len(view) != 1 || view[0].Status != status || view[0].ScreenAttention != marker {
			t.Fatalf("published %+v", view)
		}
		if first != "" && (view[0].Interaction == nil || view[0].Interaction.Options[0].Label != first) {
			t.Fatalf("bad controls: %+v", view[0])
		}
	}
	check("blocked", coordinator.ScreenWaiting, "First choice")
	conflict := p
	conflict.Status = "working"
	backend.setInventory(conflict)
	backend.events <- herdr.TopologySnapshot{}
	waitProbe(t, published)
	if a, _ := s.state.Agent(p.ID); a.RawStatus != "idle" || a.Status != "blocked" {
		t.Fatal(a)
	}
	cycle := func(content string) {
		t.Helper()
		backend.setInventory(p)
		backend.setScreen(content)
		s.poller.Wake()
		waitProbe(t, published)
		if !probe.readNext(ctx) {
			t.Fatal("no reconcile credits")
		}
	}
	cycle(strings.ReplaceAll(probeQuestion(t), "First choice", "New choice"))
	check("blocked", coordinator.ScreenWaiting, "New choice")
	cycle("Working on something else")
	check("blocked", coordinator.ScreenWaiting, "New choice")
	now = now.Add(2 * time.Second)
	cycle("Stopped waiting")
	check("done", coordinator.ScreenCleared, "")
	backend.visible = func(context.Context, string) error {
		if !s.state.CommitEventForSession(p.ID, "", "", "working", 246) {
			t.Error("working event not applied")
		}
		probe.offer(p.ID)
		return s.publishCurrentInventory(ctx)
	}
	cycle(probeQuestion(t))
	check("working", coordinator.ScreenCleared, "")
}

func TestIdleProbeNoReadsWithoutATriggerAndFailureFinishes(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		s, b, leases, probe := newProbeServer(t, enabled)
		agents := []*coordinator.AgentState{
			{PaneID: "working", TerminalID: "a", Agent: "claude", Status: "working"},
			{PaneID: "unsupported", TerminalID: "b", Agent: "unknown", Status: "idle"},
			{PaneID: "idle", TerminalID: "c", Agent: "claude", Status: "idle"},
		}
		leases.put("working", "a")
		leases.put("unsupported", "b")
		s.state.CommitInventory(agents, 0)
		if !enabled {
			if probe != nil {
				t.Fatal("worker started while disabled")
			}
			continue
		}
		probe.reconcile(true)
		if probe.readNext(t.Context()) {
			t.Fatal("no automatic reads without an eligible trigger")
		}
		s.state.CommitEvent("working", "idle", 2)
		probe.offer("working")
		b.visible = func(context.Context, string) error { return errors.New("broken read") }
		if !probe.readNext(t.Context()) {
			t.Fatal("stop was not serviced")
		}
		if a, _ := s.state.Agent("working"); a.Status != "idle" || !s.state.CompletionCurrent(a.PaneID, a.StateRevision) {
			t.Fatal(a)
		}
		s.state.CommitEvent("working", "working", 3)
		s.state.CommitEvent("working", "idle", 4)
		probe.reconcile(false)
		if a, _ := s.state.Agent("working"); a.ProbePending || !s.state.CompletionCurrent(a.PaneID, a.StateRevision) {
			t.Fatal(a)
		}
		if probe.readNext(t.Context()) {
			t.Fatal("failed reconcile retained credits")
		}
	}
}

func TestIdleProbeTransitionPublishesCommittedEvent(t *testing.T) {
	for _, scenario := range []struct {
		kind                                     string
		attentionOnly, snoozed, viewing, delayed bool
		category                                 push.Category
	}{
		{kind: "question", category: push.CategoryQuestion},
		{kind: "approval", attentionOnly: true, category: push.CategoryAttention},
		{kind: "question", attentionOnly: true},
		{kind: "question", snoozed: true},
		{kind: "question", viewing: true},
		{kind: "approval", viewing: true},
		{kind: "approval", delayed: true, category: push.CategoryAttention},
	} {
		t.Run(fmt.Sprint(scenario), func(t *testing.T) {
			s, backend, _, probe := newProbeServer(t, true)
			journal, err := activity.OpenJournal(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			s.dispatcher = coordinator.NewDispatcher(backend, s.state, journal, s.logger)
			t.Cleanup(func() { _ = s.dispatcher.Close(context.Background()) })
			pushes := transitionPushQueue(t, s)
			policy := s.pushM.Policy("device-a", "en")
			policy.Categories[push.CategoryQuestion] = !scenario.attentionOnly
			policy.Snoozed = scenario.snoozed
			if err := s.pushM.SetPolicy(policy); err != nil {
				t.Fatal(err)
			}
			s.transitionEnrich = func(context.Context, *coordinator.AgentState) { t.Error("promoted event reread") }
			s.transitionBroadcast = &recordingTransitionBroadcast{messages: make(chan any, 4)}
			a := &coordinator.AgentState{PaneID: "pane-1", TerminalID: "terminal", Agent: "claude", Status: "working"}
			s.state.CommitInventory([]*coordinator.AgentState{a}, 0)
			s.state.CommitEvent(a.PaneID, "idle", 1)
			var events []int64
			s.state.SetOnTransition(func(_, _, _, status string, rev int64) {
				if status != "blocked" {
					t.Errorf("unexpected transition %s", status)
				}
				events = append(events, rev)
			})
			frame := probeQuestion(t)
			if scenario.kind == "approval" {
				frame = probeApproval(t)
			}
			backend.setScreen(frame)
			probe.reconcile(true)
			if !probe.readNext(t.Context()) {
				t.Fatal("missing read")
			}
			if scenario.delayed {
				backend.setScreen(strings.ReplaceAll(frame, "notes.md", "second.md"))
				probe.reconcile(true)
				if !probe.readNext(t.Context()) || len(events) != 2 {
					t.Fatal(events)
				}
				s.handleTransition(t.Context(), a.PaneID, a.Agent, "", "blocked", events[0], 1)
				if len(journal.Recent(10)) != 0 || len(pushes()) != 0 {
					t.Fatal("delayed callback published replacement")
				}
			}
			if scenario.viewing {
				s.pushM.SetViewedPane("device-a", &protocol.TargetRef{
					ServerSessionID: protocol.PrimaryServerSessionID, PaneID: a.PaneID, TerminalID: a.TerminalID,
				})
			}
			last := events[len(events)-1]
			s.state.CommitEvent(a.PaneID, "idle", 2)
			s.handleTransition(t.Context(), a.PaneID, a.Agent, "", "blocked", last, 3)
			s.state.CommitEvent(a.PaneID, "blocked", 4)
			want := "blocked"
			if scenario.kind == "question" {
				want = "question"
			}
			if entries := journal.Recent(10); len(entries) != 1 || entries[0].Kind != want {
				t.Fatalf("activity: %+v", entries)
			}
			if got := pushes(); scenario.category == "" && len(got) != 0 || scenario.category != "" && (len(got) != 1 || got[0].Key.Category != scenario.category) {
				t.Fatalf("pushes: %+v", got)
			}
			if len(backend.called("ReadPane ")) != 0 {
				t.Fatal("unexpected reread")
			}
		})
	}
}

func TestIdleProbeSkipsBackgroundAndPhoneHistory(t *testing.T) {
	for _, agent := range []string{"claude", "qoder"} {
		t.Run(agent, func(t *testing.T) {
			s, backend, _, probe := newProbeServer(t, true)
			s.historyTasks = newLifecycleTasks(t.Context())
			t.Cleanup(s.historyTasks.Stop)
			a := &coordinator.AgentState{PaneID: "pane-1", TerminalID: "terminal", Agent: agent, Status: "working"}
			frame := probeQuestion(t)
			if agent == "qoder" {
				frame = probeFixture(t, "qodercli-second-question-single", "Tab/←→ switch · Enter select · Esc back", "Tab/←→ switch · Enter select · Esc back")
			}
			if agent == "claude" && question.LayoutHint(frame) {
				t.Fatal("fixture unexpectedly matched ordinary layout")
			}
			backend.setScreen("previous output\nthis is an inflight capture\nfooter 1\nfooter 2\nfooter 3\nfooter 4\nfooter 5\nfooter 6")
			entered, release := make(chan struct{}), make(chan struct{})
			backend.history = func(ctx context.Context) error {
				close(entered)
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			s.state.CommitInventory([]*coordinator.AgentState{a}, 0)
			s.historyM.Merge(a.PaneID, "prior history\nfooter 1\nfooter 2\nfooter 3\nfooter 4\nfooter 5\nfooter 6")
			before := s.historyM.Content(a.PaneID, 100)
			publishInventoryForTest(t, s)
			waitProbe(t, entered)
			s.state.CommitEvent(a.PaneID, "idle", 1)
			backend.setScreen(frame)
			probe.reconcile(true)
			if !probe.readNext(t.Context()) {
				t.Fatal("expected probe")
			}
			if promoted, _ := s.state.Agent(a.PaneID); promoted.ScreenAttention != coordinator.ScreenWaiting {
				t.Fatalf("expected promotion: %+v", promoted)
			}
			close(release)
			for range 3 {
				publishInventoryForTest(t, s)
				s.scheduleActiveHistoryCaptures(t.Context())
				s.classifyPaneResponse(coordinator.PaneReadRequest{PaneID: a.PaneID, Lines: 100}, map[string]any{"content": frame})
			}
			s.historyTasks.Stop()
			if reads := backend.called("ReadPane "); !reflect.DeepEqual(reads, []string{"ReadPane pane-1 lines=10000 format=ansi"}) {
				t.Fatal(reads)
			}
			if got := s.historyM.Content(a.PaneID, 100); got != before {
				t.Fatalf("history changed: %s", got)
			}
		})
	}
}
