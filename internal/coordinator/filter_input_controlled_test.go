package coordinator

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

type filterSocketWrite struct {
	Pane   string
	Method string
	Text   string
}

type controlledFilterSocket struct {
	mu                  sync.Mutex
	accepted            map[string]string
	visible             map[string]string
	writes              []filterSocketWrite
	reads               map[string]int
	deferRepaint        bool
	failReadsAfterWrite bool
}

func newControlledFilterDispatcher(t *testing.T) (*Dispatcher, *controlledFilterSocket) {
	t.Helper()
	fake := &controlledFilterSocket{accepted: make(map[string]string), visible: make(map[string]string), reads: make(map[string]int)}
	path := filepath.Join(t.TempDir(), "native.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	var connections sync.Map
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Store(conn, true)
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer connections.Delete(conn)
				defer conn.Close()
				decoder := json.NewDecoder(bufio.NewReader(conn))
				for {
					var request struct {
						ID     string `json:"id"`
						Method string `json:"method"`
						Params struct {
							Pane string   `json:"pane_id"`
							Text string   `json:"text"`
							Keys []string `json:"keys"`
						} `json:"params"`
					}
					if decoder.Decode(&request) != nil {
						return
					}
					fake.mu.Lock()
					pane := request.Params.Pane
					response := map[string]any{"id": request.ID, "result": map[string]any{"type": "ok"}}
					switch request.Method {
					case "pane.read":
						fake.reads[pane]++
						if fake.failReadsAfterWrite && fake.accepted[pane] != "" {
							delete(response, "result")
							response["error"] = map[string]any{"code": "read_failed", "message": "controlled read failure"}
						} else {
							response["result"] = map[string]any{"type": "pane_read", "read": map[string]any{"text": filterScreen(fake.visible[pane])}}
						}
					case "pane.send_keys":
						text := ""
						if len(request.Params.Keys) == 1 {
							text = request.Params.Keys[0]
						}
						if text == "Space" {
							text = " "
						}
						fake.writes = append(fake.writes, filterSocketWrite{pane, request.Method, text})
						if len(text) == 1 {
							fake.accepted[pane] += text
							if !fake.deferRepaint {
								fake.visible[pane] = fake.accepted[pane]
							}
						}
					case "pane.send_input":
						fake.writes = append(fake.writes, filterSocketWrite{pane, request.Method, request.Params.Text})
					default:
						delete(response, "result")
						response["error"] = map[string]any{"code": "unknown_method", "message": "unsupported test method"}
					}
					fake.mu.Unlock()
					if json.NewEncoder(conn).Encode(response) != nil {
						return
					}
				}
			}()
		}
	}()
	client := herdr.NewClient(filepath.Join(t.TempDir(), "no-cli-fallback"), path)
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Agent: "cursor", Status: "working"}, {PaneID: "pane-2", Agent: "cursor", Status: "working"}}, state.RevisionCounter())
	d := NewDispatcher(client, state, nil, testLogger())
	t.Cleanup(func() {
		_ = listener.Close()
		<-stopped
		connections.Range(func(key, value any) bool { _ = key.(net.Conn).Close(); return true })
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := d.Close(ctx); err != nil {
			t.Errorf("dispatcher cleanup: %v", err)
		}
		_ = client.Close()
		workers.Wait()
	})
	return d, fake
}

func (f *controlledFilterSocket) recordedWrites() []filterSocketWrite {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]filterSocketWrite(nil), f.writes...)
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
	result := d.Handle(context.Background(), map[string]any{"action": "send_filter_text", "pane_id": "pane-1", "text": "gr"})
	if !result.OK {
		t.Fatalf("delayed repaint: %+v", result)
	}
	wantWaits := []time.Duration{150 * time.Millisecond, 25 * time.Millisecond, 150 * time.Millisecond, 25 * time.Millisecond}
	if !reflect.DeepEqual(waits, wantWaits) {
		t.Fatalf("waits=%v, want %v", waits, wantWaits)
	}
	want := []filterSocketWrite{{"pane-1", "pane.send_keys", "g"}, {"pane-1", "pane.send_keys", "r"}}
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
	result := d.Handle(context.Background(), map[string]any{"action": "send_filter_text", "pane_id": "pane-1", "text": "grok"})
	assertFilterUnknown(t, result)
	want := []filterSocketWrite{{"pane-1", "pane.send_keys", "g"}}
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
		results <- d.Handle(context.Background(), map[string]any{
			"action": "send_filter_text", "pane_id": "pane-1", "text": "gr",
			"_server_received_at": receivedAt.Format(time.RFC3339Nano),
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
	want := []filterSocketWrite{{"pane-1", "pane.send_keys", "g"}}
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
		filterResult <- d.Handle(context.Background(), map[string]any{"action": "send_filter_text", "pane_id": "pane-1", "text": "gr"})
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("filter never reached pacing")
	}
	admitted := make(chan struct{})
	sameCtx := context.WithValue(context.Background(), admissionContextKey{}, func() { close(admitted) })
	sameResult := make(chan *CommandResult, 1)
	go func() {
		sameResult <- d.Handle(sameCtx, map[string]any{"action": "send_input", "pane_id": "pane-1", "text": "same"})
	}()
	select {
	case <-admitted:
	case <-time.After(3 * time.Second):
		t.Fatal("same-pane command was not admitted")
	}
	otherResult := make(chan *CommandResult, 1)
	go func() {
		otherResult <- d.Handle(context.Background(), map[string]any{"action": "send_input", "pane_id": "pane-2", "text": "other"})
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
	beforeRelease := []filterSocketWrite{{"pane-1", "pane.send_keys", "g"}, {"pane-2", "pane.send_input", "other"}}
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
	want := append(beforeRelease, filterSocketWrite{"pane-1", "pane.send_keys", "r"}, filterSocketWrite{"pane-1", "pane.send_input", "same"})
	if got := fake.recordedWrites(); !reflect.DeepEqual(got, want) {
		t.Fatalf("FIFO order=%+v, want %+v", got, want)
	}
}
