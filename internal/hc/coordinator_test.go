package hc_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/hc"
)

func TestCoordinatorIdentity(t *testing.T) {
	var mu sync.Mutex
	var subscription net.Conn
	pid := 10
	client := hc.New(hc.FakeCore(t, 1, func(conn net.Conn, req hc.FakeRequest) {
		mu.Lock()
		defer mu.Unlock()
		switch req.Method {
		case "events.subscribe":
			subscription = conn
			hc.Reply(conn, req.ID, "result", map[string]any{})
		case "terminal.list":
			hc.Reply(conn, req.ID, "result", json.RawMessage(fmt.Sprintf(`{"terminals":[{"address":"t1","pid":%d,"state":"live","foreground":{"name":"pi"}},{"address":"t2","pid":20,"state":"live","foreground":{"name":"pi"}},{"address":"no-pid","state":"live","foreground":{"name":"pi"}}]}`, pid)))
		case "doc.get":
			hc.Reply(conn, req.ID, "result", json.RawMessage(`{"value":{}}`))
		default:
			t.Errorf("unexpected method: %s", req.Method)
		}
	}))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	state := coordinator.NewState(logger)
	poller := coordinator.NewPoller(client, state, time.Hour, logger)
	poller.SetEventReconnectWait(func(ctx context.Context) bool { return ctx.Err() == nil })
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); poller.RunEvents(ctx) }()
	defer func() { cancel(); <-done }()
	wait := func(label string, ready func() bool) {
		t.Helper()
		deadline := time.NewTimer(5 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for !ready() {
			select {
			case <-deadline.C:
				t.Fatalf("timed out: %s; state=%+v", label, state.Snapshot())
			case <-tick.C:
			}
		}
		if _, ok := state.Agent("no-pid"); ok {
			t.Fatal("live terminal without pid entered state")
		}
	}
	push := func(kind string, seq int) {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		if _, err := fmt.Fprintln(subscription, hc.EventLine(kind, "t1", seq)); err != nil {
			t.Fatal(err)
		}
	}
	wait("initial snapshot", func() bool { return state.AgentCount() == 2 })
	first, other := state.Generation("t1"), state.Generation("t2")
	push("status", 1)
	wait("status committed", func() bool { a, ok := state.Agent("t1"); return ok && a.PaneRevision == 1 })
	if state.Generation("t1") != first || state.Generation("t2") != other {
		t.Fatal("status changed generation")
	}
	for i, kind := range []string{"lost", "restarted", "finished", "removed"} {
		before := state.Generation("t1")
		push(kind, i+2)
		wait(kind, func() bool { return state.Generation("t1") > before })
		if state.Generation("t2") != other {
			t.Fatalf("%s changed unrelated pane generation", kind)
		}
	}
	before := state.Generation("t1")
	push("missed", 6)
	wait("missed reconnect", func() bool { return state.Generation("t1") > before && state.Generation("t2") > other })
	before, other = state.Generation("t1"), state.Generation("t2")
	mu.Lock()
	pid = 11
	mu.Unlock()
	pollCtx, stopPoll := context.WithCancel(ctx)
	pollDone := make(chan struct{})
	go func() { defer close(pollDone); poller.Run(pollCtx) }()
	defer func() { stopPoll(); <-pollDone }()
	wait("inventory-only pid replacement", func() bool { return state.Generation("t1") > before })
	if state.Generation("t2") != other {
		t.Fatal("pid replacement changed unrelated pane generation")
	}
}
