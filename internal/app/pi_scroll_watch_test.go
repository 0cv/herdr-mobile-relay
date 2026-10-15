package app

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/panesize"
	"github.com/0cv/herdr-mobile-relay/internal/piscroll"
	"github.com/0cv/herdr-mobile-relay/internal/transport"

	"github.com/0cv/herdr-mobile-relay/internal/protocol"
)

func TestScrollStateInvalidationPrecedesUnchangedText(t *testing.T) {
	old := &paneWatchFrame{content: "unchanged\n", contentFingerprint: "same", frameFingerprint: "old", classificationAgent: "pi", scrollRevision: "old"}
	if !paneWatchNeedsFrameRead("same", "same", old, "pi", true) {
		t.Fatal("same text hid a state invalidation")
	}
	if paneWatchNeedsFrameRead("same", "same", old, "pi", false) {
		t.Fatal("unchanged state forced an unnecessary frame")
	}
	previous := map[string]any{"type": "pane_content", "content": "unchanged\n", "scrolling": &protocol.ScrollState{Revision: strings.Repeat("a", 64), Route: "application", Renderer: "fullscreen"}}
	current := map[string]any{"type": "pane_content", "content": "unchanged\n", "scrolling": &protocol.ScrollState{Revision: strings.Repeat("b", 64), Route: "local", Renderer: "regular"}}
	old.frameFingerprint = paneFrameFingerprint(previous)
	next := &paneWatchFrame{content: old.content, contentFingerprint: old.contentFingerprint, frameFingerprint: paneFrameFingerprint(current)}
	if next.frameFingerprint == old.frameFingerprint {
		t.Fatal("state omitted from frame identity")
	}
	delta := paneWatchUpdate(current, old, next)
	if delta == nil || delta["type"] != "pane_delta" || delta["scrolling"] != current["scrolling"] {
		t.Fatalf("metadata-only state frame lost: %+v", delta)
	}
	if paneWatchUpdate(current, next, next) != nil {
		t.Fatal("acknowledged identical frame was repeated")
	}
}
func TestScrollRefreshRequiresPossibleScreenMutation(t *testing.T) {
	for _, phase := range []string{"completed", "dispatched_unknown", "not_started", "failed"} {
		t.Run(phase, func(t *testing.T) {
			first := &paneWatch{paneID: "pane", refresh: make(chan struct{}, 1)}
			second := &paneWatch{paneID: "pane", refresh: make(chan struct{}, 1)}
			server := &Server{paneWatches: map[string]*paneWatch{"first": first, "second": second}}
			for i := 0; i < 100; i++ {
				server.refreshPaneWatchesAfterInput("scroll_agent", "pane", &coordinator.CommandResult{OK: phase == "completed", Phase: phase})
			}
			want := phase == "completed" || phase == "dispatched_unknown"
			for _, watch := range []*paneWatch{first, second} {
				if (len(watch.refresh) == 1) != want || watch.forceRefresh != want {
					t.Fatalf("phase %s refreshed=%v", phase, watch.forceRefresh)
				}
			}
		})
	}
}

type watchScrollResolver func(context.Context, piscroll.Target, bool) (piscroll.Evidence, error)

func (f watchScrollResolver) Resolve(ctx context.Context, target piscroll.Target, measure bool) (piscroll.Evidence, error) {
	return f(ctx, target, measure)
}

type watchGeometryRunner struct{ calls map[string]int }

func (r watchGeometryRunner) Output(_ context.Context, name string, _ ...string) ([]byte, error) {
	r.calls[name]++
	if name == "ps" {
		return []byte("pts/7"), nil
	}
	return []byte("30 80"), nil
}

func TestOrdinaryPaneRevisionDoesNotInvalidateScrollGeometry(t *testing.T) {
	ctx := context.Background()
	state := coordinator.NewState(slog.Default())
	agent := &coordinator.AgentState{PaneID: "pane", TerminalID: "terminal", Agent: "pi", SessionID: "session"}
	state.CommitInventory([]*coordinator.AgentState{agent}, 0)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := herdr.NewClient("/must-not-run", "")
	defer client.Close()
	dispatcher := coordinator.NewDispatcher(client, state, nil, logger)
	defer dispatcher.Close(ctx)
	calls := map[string]int{}
	reader := panesize.NewReader(watchGeometryRunner{calls}, "linux")
	manager := piscroll.New(watchScrollResolver(func(ctx context.Context, _ piscroll.Target, measure bool) (piscroll.Evidence, error) {
		e := piscroll.Evidence{Instance: "instance", Incarnation: "bridge", PID: 42, Renderer: "fullscreen", Wheel: "sgr_cell"}
		if measure {
			var err error
			e.Grid, err = reader.Read(ctx, 42)
			return e, err
		}
		return e, nil
	}))
	dispatcher.SetScrollManager(manager)
	initial := dispatcher.ScrollState(ctx, "pane", "", true)
	if !initial.Available {
		t.Fatalf("initial: %+v", initial)
	}
	watch := &paneWatch{client: &transport.ClientConn{}, paneID: "pane", ctx: ctx, pending: &paneWatchFrame{scrollRevision: initial.Revision}}
	server := &Server{state: state, dispatcher: dispatcher, paneWatches: map[string]*paneWatch{"": watch}}
	time.Sleep(510 * time.Millisecond)
	for i := 1; i <= 10; i++ {
		next, _ := state.Agent("pane")
		next.PaneRevision = i
		next.ScrollMaxOffset = i
		state.CommitInventory([]*coordinator.AgentState{next}, 0)
		observed, _ := state.Agent("pane")
		if observed.PaneRevision != i {
			t.Fatal("fixture did not publish ordinary output revision")
		}
		server.pollPaneWatch(watch)
		got := dispatcher.ScrollState(ctx, "pane", "", false)
		if got.Revision != initial.Revision || got.GeometryEpoch != initial.GeometryEpoch || calls["ps"] != 1 || calls["stty"] != 1 {
			t.Fatalf("ordinary output changed geometry/cost: %+v calls=%v", got, calls)
		}
	}
}

func TestPostInputRefreshCoalescesForAllPaneViewers(t *testing.T) {
	makeWatch := func(pane string) *paneWatch { return &paneWatch{paneID: pane, refresh: make(chan struct{}, 1)} }
	first, second, other := makeWatch("pane"), makeWatch("pane"), makeWatch("other")
	server := &Server{paneWatches: map[string]*paneWatch{"first": first, "second": second, "other": other}}
	for i := 0; i < 100; i++ {
		server.refreshPaneWatches("pane")
	}
	if len(first.refresh) != 1 || len(second.refresh) != 1 || len(other.refresh) != 0 || !first.forceRefresh || !second.forceRefresh {
		t.Fatal("refresh backlogged or missed a viewer")
	}
	for _, action := range []string{"submit_prompt", "send_keys", "send_text", "send_input", "send_filter_text", "send_secret", "respond", "answer_question", "clarify_question", "navigate_question", "scroll_agent"} {
		if !paneInputChangesScreen(action) {
			t.Fatalf("missing post-input refresh for %s", action)
		}
	}
	for _, action := range []string{"read_pane", "watch_pane", "pane_applied", "list_conversation_messages", "set_pane_size", "release_pane_size", "approve_action", "submit_answer"} {
		if paneInputChangesScreen(action) {
			t.Fatalf("read-only/geometry activity treated as agent input: %s", action)
		}
	}
}
