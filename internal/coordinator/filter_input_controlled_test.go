package coordinator

import (
	"context"
	"errors"
	"io"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
)

type filterWrite struct {
	Pane   string
	Method string
	Text   string
}

type controlledFilterScreen struct {
	backend             *fakeBackend
	mu                  sync.Mutex
	accepted            map[string]string
	visible             map[string]string
	reads               map[string]int
	deferRepaint        bool
	failReadsAfterWrite bool
	dropInputReply      bool
}

func newControlledFilterDispatcher(t *testing.T) (*Dispatcher, *controlledFilterScreen) {
	t.Helper()
	fake := &controlledFilterScreen{
		backend:  newFakeBackend(),
		accepted: make(map[string]string),
		visible:  make(map[string]string),
		reads:    make(map[string]int),
	}
	fake.backend.setHook("ReadPaneVisible", func(_ context.Context, call fakeCall) error {
		pane := call.Args[0].(string)
		fake.mu.Lock()
		defer fake.mu.Unlock()
		fake.reads[pane]++
		if fake.failReadsAfterWrite && fake.accepted[pane] != "" {
			return &herdr.CLIError{Code: "read_failed", Message: "controlled read failure"}
		}
		fake.backend.update(func(b *fakeBackend) { b.contents[pane] = filterScreen(fake.visible[pane]) })
		return nil
	})
	fake.backend.setHook("SendLiteralKey", func(_ context.Context, call fakeCall) error {
		pane, character := call.Args[0].(string), call.Args[1].(string)
		fake.mu.Lock()
		defer fake.mu.Unlock()
		fake.accepted[pane] += character
		if !fake.deferRepaint {
			fake.visible[pane] = fake.accepted[pane]
		}
		return nil
	})
	fake.backend.setHook("SendInput", func(context.Context, fakeCall) error {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		if fake.dropInputReply {
			return &herdr.OutcomeError{Started: true, Err: io.EOF}
		}
		return nil
	})
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Agent: "cursor", Status: "working"}, {PaneID: "pane-2", Agent: "cursor", Status: "working"}}, state.RevisionCounter())
	return newTestDispatcher(t, fake.backend, state), fake
}

func (f *controlledFilterScreen) recordedWrites() []filterWrite {
	var writes []filterWrite
	for _, call := range f.backend.recorded() {
		switch call.Method {
		case "SendLiteralKey":
			writes = append(writes, filterWrite{call.Args[0].(string), call.Method, call.Args[1].(string)})
		case "SendInput":
			writes = append(writes, filterWrite{call.Args[0].(string), call.Method, call.Args[1].(herdr.PaneInput).Text})
		}
	}
	return writes
}

func receiveFilterResult(t *testing.T, result <-chan *CommandResult) *CommandResult {
	t.Helper()
	select {
	case value := <-result:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("filter operation did not finish")
		return nil
	}
}

func assertFilterUnknown(t *testing.T, result *CommandResult) {
	t.Helper()
	if result.OK || result.Phase != "dispatched_unknown" {
		t.Fatalf("unsafe filter result: %+v", result)
	}
	data, ok := result.Data.(map[string]any)
	if !ok || data["dispatched_unknown"] != true {
		t.Fatalf("missing uncertainty data: %+v", result)
	}
}

func TestFilterDelayedRepaintConfirmsWithoutResending(t *testing.T) {
	d, fake := newControlledFilterDispatcher(t)
	fake.deferRepaint = true
	var waits []time.Duration
	d.testFilterWait = func(ctx context.Context, duration time.Duration) error {
		waits = append(waits, duration)
		if duration == 25*time.Millisecond {
			fake.mu.Lock()
			fake.visible["pane-1"] = fake.accepted["pane-1"]
			fake.mu.Unlock()
		}
		return ctx.Err()
	}
	result := d.Handle(context.Background(), protocol.Inbound{Type: "send_filter_text", PaneID: "pane-1", Text: "gr"})
	if !result.OK {
		t.Fatalf("delayed repaint: %+v", result)
	}
	wantWaits := []time.Duration{150 * time.Millisecond, 25 * time.Millisecond, 150 * time.Millisecond, 25 * time.Millisecond}
	if !reflect.DeepEqual(waits, wantWaits) {
		t.Fatalf("waits=%v, want %v", waits, wantWaits)
	}
	want := []filterWrite{{"pane-1", "SendLiteralKey", "g"}, {"pane-1", "SendLiteralKey", "r"}}
	if got := fake.recordedWrites(); !reflect.DeepEqual(got, want) {
		t.Fatalf("writes=%+v, want %+v", got, want)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.reads["pane-1"] != 7 {
		t.Fatalf("reads=%d, expected initial, pre-write and delayed confirmation reads", fake.reads["pane-1"])
	}
}

func TestFilterReadFailureAfterPrefixIsUnknown(t *testing.T) {
	d, fake := newControlledFilterDispatcher(t)
	fake.failReadsAfterWrite = true
	d.testFilterWait = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	result := d.Handle(context.Background(), protocol.Inbound{Type: "send_filter_text", PaneID: "pane-1", Text: "grok"})
	assertFilterUnknown(t, result)
	want := []filterWrite{{"pane-1", "SendLiteralKey", "g"}}
	if got := fake.recordedWrites(); !reflect.DeepEqual(got, want) {
		t.Fatalf("continued or retried after read failure: %+v", got)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.reads["pane-1"] < 3 {
		t.Fatalf("no post-write read was attempted: %d", fake.reads["pane-1"])
	}
}

func TestFilterSchedulerDeadlineAfterPrefixIsUnknown(t *testing.T) {
	d, fake := newControlledFilterDispatcher(t)
	entered := make(chan context.Context, 1)
	d.testFilterWait = func(ctx context.Context, _ time.Duration) error {
		entered <- ctx
		<-ctx.Done()
		return ctx.Err()
	}
	receivedAt := time.Now().Add(-commandDeadline + 2*time.Second)
	results := make(chan *CommandResult, 1)
	go func() {
		results <- d.Handle(context.Background(), protocol.Inbound{
			Type:       "send_filter_text",
			PaneID:     "pane-1",
			Text:       "gr",
			ReceivedAt: receivedAt,
		})
	}()
	var effectCtx context.Context
	select {
	case effectCtx = <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("filter never reached pacing after first write")
	}
	deadline, ok := effectCtx.Deadline()
	if !ok || !deadline.Equal(receivedAt.Add(commandDeadline)) {
		t.Fatalf("effect deadline=%v, want scheduler deadline %v", deadline, receivedAt.Add(commandDeadline))
	}
	result := receiveFilterResult(t, results)
	assertFilterUnknown(t, result)
	if !errors.Is(effectCtx.Err(), context.DeadlineExceeded) {
		t.Fatalf("not a scheduler deadline expiry: %v", effectCtx.Err())
	}
	want := []filterWrite{{"pane-1", "SendLiteralKey", "g"}}
	if got := fake.recordedWrites(); !reflect.DeepEqual(got, want) {
		t.Fatalf("continued after deadline: %+v", got)
	}
}

func TestFilterHoldsPaneFIFOWhileOtherPaneRemainsUsable(t *testing.T) {
	for _, pause := range []time.Duration{150 * time.Millisecond, 25 * time.Millisecond} {
		t.Run(pause.String(), func(t *testing.T) { testFilterFIFOWhilePaused(t, pause) })
	}
}

func testFilterFIFOWhilePaused(t *testing.T, pause time.Duration) {
	t.Helper()
	d, fake := newControlledFilterDispatcher(t)
	fake.deferRepaint = true
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	paused := false
	d.testFilterWait = func(ctx context.Context, duration time.Duration) error {
		if duration == pause && !paused {
			paused = true
			entered <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if duration == 25*time.Millisecond {
			fake.mu.Lock()
			fake.visible["pane-1"] = fake.accepted["pane-1"]
			fake.mu.Unlock()
		}
		return ctx.Err()
	}
	filterResult := make(chan *CommandResult, 1)
	go func() {
		filterResult <- d.Handle(context.Background(), protocol.Inbound{Type: "send_filter_text", PaneID: "pane-1", Text: "gr"})
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("filter never reached pacing")
	}
	admitted := make(chan struct{})
	sameResult := make(chan *CommandResult, 1)
	go func() {
		sameResult <- d.HandleAdmitted(context.Background(), protocol.Inbound{Type: "send_input", PaneID: "pane-1", Text: "same"}, func() { close(admitted) })
	}()
	select {
	case <-admitted:
	case <-time.After(3 * time.Second):
		t.Fatal("same-pane command was not admitted")
	}
	otherResult := make(chan *CommandResult, 1)
	go func() {
		otherResult <- d.Handle(context.Background(), protocol.Inbound{Type: "send_input", PaneID: "pane-2", Text: "other"})
	}()
	if result := receiveFilterResult(t, otherResult); !result.OK {
		t.Fatalf("other pane blocked: %+v", result)
	}
	readCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := d.herdr.ReadPaneVisible(readCtx, "pane-2", 200, "text"); err != nil {
		t.Fatalf("other pane read blocked during pacing: %v", err)
	}
	select {
	case result := <-sameResult:
		t.Fatalf("same-pane command interleaved: %+v", result)
	default:
	}
	beforeRelease := []filterWrite{{"pane-1", "SendLiteralKey", "g"}, {"pane-2", "SendInput", "other"}}
	if got := fake.recordedWrites(); !reflect.DeepEqual(got, beforeRelease) {
		t.Fatalf("writes while filter paused: %+v", got)
	}
	unblock()
	if result := receiveFilterResult(t, filterResult); !result.OK {
		t.Fatalf("filter failed: %+v", result)
	}
	if result := receiveFilterResult(t, sameResult); !result.OK {
		t.Fatalf("queued command failed: %+v", result)
	}
	want := append(beforeRelease, filterWrite{"pane-1", "SendLiteralKey", "r"}, filterWrite{"pane-1", "SendInput", "same"})
	if got := fake.recordedWrites(); !reflect.DeepEqual(got, want) {
		t.Fatalf("FIFO order=%+v, want %+v", got, want)
	}
}
