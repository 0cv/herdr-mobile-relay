package coordinator

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/panesize"
	"github.com/0cv/herdr-mobile-relay/internal/pibridge"
	"github.com/0cv/herdr-mobile-relay/internal/piscroll"
)

type scrollFixture struct {
	d                              *Dispatcher
	mu                             sync.Mutex
	methods                        map[string]int
	texts                          []string
	grid                           panesize.Grid
	renderer, incarnation, rawMode string
	supported                      atomic.Bool
	bridgeCalls, geometryCalls     atomic.Int32
	bridgeHook                     func()
	rawHook                        func()
	geometryHook                   func()
	processHook                    func(int)
	readHook                       func(int)
	processDelay, rawDelay         time.Duration
	editorAt                       int
}

func newScrollFixture(t *testing.T) *scrollFixture {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "scroll-dispatch-")
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "api.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	f := &scrollFixture{methods: map[string]int{}, grid: panesize.Grid{TTY: "/dev/tty1", Rows: 30, Columns: 79}, renderer: "fullscreen", incarnation: "bridge"}
	f.supported.Store(true)
	var wg sync.WaitGroup
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				decoder := json.NewDecoder(bufio.NewReader(conn))
				for {
					var request struct {
						ID     string         `json:"id"`
						Method string         `json:"method"`
						Params map[string]any `json:"params"`
					}
					if decoder.Decode(&request) != nil {
						return
					}
					f.mu.Lock()
					f.methods[request.Method]++
					count := f.methods[request.Method]
					delay, rawDelay, mode, editorAt := f.processDelay, f.rawDelay, f.rawMode, f.editorAt
					processHook, readHook, rawHook := f.processHook, f.readHook, f.rawHook
					if request.Method == "pane.send_text" {
						f.texts = append(f.texts, request.Params["text"].(string))
					}
					f.mu.Unlock()
					result := map[string]any{"type": "ok"}
					switch request.Method {
					case "pane.process_info":
						if delay > 0 {
							time.Sleep(delay)
						}
						processes := []herdr.PaneProcess{{PID: 42}}
						if editorAt > 0 && count >= editorAt {
							processes = append(processes, herdr.PaneProcess{PID: 43, Name: "vim"})
						}
						result = map[string]any{"type": "pane_process_info", "process_info": herdr.PaneProcessInfo{PaneID: "pane", ShellPID: 1, ForegroundProcessGroupID: 42, ForegroundProcesses: processes}}
						if processHook != nil {
							processHook(count)
						}
					case "pane.send_text":
						if rawHook != nil {
							rawHook()
						}
						if rawDelay > 0 {
							time.Sleep(rawDelay)
						}
						if mode == "drop" {
							return
						}
					case "pane.read":
						if readHook != nil {
							readHook(count)
						}
						result = map[string]any{"type": "pane_read", "read": map[string]any{"text": "unchanged\n", "truncated": false}}
					default:
						t.Errorf("unexpected socket operation: %s", request.Method)
					}
					if json.NewEncoder(conn).Encode(map[string]any{"id": request.ID, "result": result}) != nil {
						return
					}
				}
			}()
		}
	}()
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{PaneID: "pane", TerminalID: "terminal", SessionID: "session", Agent: "pi", Status: "idle"}}, 0)
	client := herdr.NewClient("/must-not-run-cli", socket)
	f.d = NewDispatcher(client, state, nil, testLogger())
	resolver := &piscroll.PiResolver{Process: client.SocketPaneProcessInfo, Instance: func() (string, error) { return "instance", nil }, Supported: f.supported.Load, Current: func(target piscroll.Target) bool {
		current, ok := f.d.scrollTarget("pane")
		return ok && current == target
	},
		Bridge: func(ctx context.Context, id pibridge.Identity) (pibridge.ScrollResponse, error) {
			f.bridgeCalls.Add(1)
			f.mu.Lock()
			hook, renderer, inc := f.bridgeHook, f.renderer, f.incarnation
			f.mu.Unlock()
			if hook != nil {
				hook()
			}
			wheel := "sgr_cell"
			if renderer != "fullscreen" {
				wheel = "unavailable"
			}
			return pibridge.ScrollResponse{Identity: id, Incarnation: inc, Renderer: renderer, Wheel: wheel}, ctx.Err()
		},
		Geometry: func(ctx context.Context, pid int) (panesize.Grid, error) {
			f.geometryCalls.Add(1)
			f.mu.Lock()
			grid, hook := f.grid, f.geometryHook
			f.mu.Unlock()
			if hook != nil {
				hook()
			}
			return grid, ctx.Err()
		},
	}
	f.d.piResolver = resolver
	f.d.scroll = piscroll.New(resolver)
	t.Cleanup(func() {
		_ = f.d.Close(context.Background())
		_ = client.Close()
		_ = listener.Close()
		wg.Wait()
		_ = os.RemoveAll(dir)
	})
	target, _ := f.d.scrollTarget("pane")
	if err := f.d.scroll.Validate(context.Background(), target, true); err != nil {
		t.Fatal(err)
	}
	f.reset()
	return f
}
func (f *scrollFixture) reset() {
	f.mu.Lock()
	f.methods = map[string]int{}
	f.texts = nil
	f.mu.Unlock()
	f.bridgeCalls.Store(0)
	f.geometryCalls.Store(0)
}
func (f *scrollFixture) snapshot(owner string) string {
	return f.d.ScrollState(context.Background(), "pane", owner, false).Revision
}
func (f *scrollFixture) message(kind, id, owner string) map[string]any {
	return map[string]any{"action": kind, "pane_id": "pane", "request_id": id, "_scroll_owner": owner, "state_revision": f.snapshot(owner), "direction": "up"}
}
func (f *scrollFixture) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.methods[method]
}
func TestScrollFreshBindingBudgetAndReplay(t *testing.T) {
	f := newScrollFixture(t)
	message := f.message("scroll_agent", "wheel", "owner")
	if result := f.d.Handle(context.Background(), message); !result.OK {
		t.Fatalf("wheel: %+v", result)
	}
	if f.count("pane.process_info") != 2 || f.count("pane.send_text") != 1 || f.bridgeCalls.Load() != 1 || f.geometryCalls.Load() != 1 {
		t.Fatalf("budget: %+v %d/%d", f.methods, f.bridgeCalls.Load(), f.geometryCalls.Load())
	}
	f.mu.Lock()
	text := f.texts[0]
	f.mu.Unlock()
	if text != "\x1b[<64;40;15M" {
		t.Fatalf("raw report: %q", text)
	}
	if result := f.d.Handle(context.Background(), message); !result.OK || f.count("pane.send_text") != 1 || f.bridgeCalls.Load() != 1 {
		t.Fatalf("replayed wheel: %+v", result)
	}
	message["direction"] = "down"
	if result := f.d.Handle(context.Background(), message); result.OK || f.count("pane.send_text") != 1 {
		t.Fatalf("conflicting ID: %+v", result)
	}
}
func TestScrollSelectionDeduplicatesAndChecksRevision(t *testing.T) {
	f := newScrollFixture(t)
	message := f.message("set_scroll_selection", "select", "owner")
	message["selection"] = "local"
	if result := f.d.Handle(context.Background(), message); !result.OK {
		t.Fatalf("selection: %+v", result)
	}
	revision := f.snapshot("owner")
	if result := f.d.Handle(context.Background(), message); !result.OK || revision != f.snapshot("owner") {
		t.Fatalf("duplicate changed selection: %+v", result)
	}
	message["selection"] = "auto"
	if result := f.d.Handle(context.Background(), message); result.OK {
		t.Fatal("conflicting selection ID accepted")
	}
	message["request_id"] = "old-revision"
	if result := f.d.Handle(context.Background(), message); result.OK {
		t.Fatal("stale selection accepted")
	}
	if state := f.d.ScrollState(context.Background(), "pane", "other", false); state.Selection != "auto" {
		t.Fatal("selection leaked to another owner")
	}
	if f.count("pane.send_text") != 0 {
		t.Fatal("selection sent terminal input")
	}
}
func TestScrollRejectsChangedEvidence(t *testing.T) {
	for _, change := range []string{"geometry", "regular", "bridge", "editor-before", "editor-after", "session", "capability"} {
		t.Run(change, func(t *testing.T) {
			f := newScrollFixture(t)
			message := f.message("scroll_agent", "reject", "owner")
			f.mu.Lock()
			switch change {
			case "geometry":
				f.grid.Columns++
			case "regular":
				f.renderer = "regular"
			case "bridge":
				f.incarnation = "replacement"
			case "editor-before":
				f.editorAt = 1
			case "editor-after":
				f.editorAt = 2
			case "session":
				f.bridgeHook = func() {
					f.d.state.CommitInventory([]*AgentState{{PaneID: "pane", TerminalID: "terminal", SessionID: "replacement", Agent: "pi", Status: "idle"}}, f.d.state.Revision("pane"))
				}
			case "capability":
				f.bridgeHook = func() { f.supported.Store(false) }
			}
			f.mu.Unlock()
			if result := f.d.Handle(context.Background(), message); result.OK || result.Phase != "not_started" {
				t.Fatalf("changed evidence: %+v", result)
			}
			if f.count("pane.send_text") != 0 {
				t.Fatal("changed evidence wrote input")
			}
		})
	}
}
func TestScrollDeadlineAndAuthorizationFences(t *testing.T) {
	for _, change := range []string{"expired", "slow-preflight", "revoked", "cancelled"} {
		t.Run(change, func(t *testing.T) {
			f := newScrollFixture(t)
			message := f.message("scroll_agent", "fenced", "owner")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var revoked atomic.Bool
			ctx = herdr.WithDispatchCheck(ctx, func() error {
				if revoked.Load() {
					return errors.New("controller revoked")
				}
				return nil
			})
			f.mu.Lock()
			switch change {
			case "expired":
				message["_server_received_at"] = time.Now().Add(-300 * time.Millisecond).Format(time.RFC3339Nano)
			case "slow-preflight":
				f.processDelay = 150 * time.Millisecond
			case "revoked":
				f.processHook = func(count int) {
					if count == 2 {
						revoked.Store(true)
					}
				}
			case "cancelled":
				f.bridgeHook = cancel
			}
			f.mu.Unlock()
			started := time.Now()
			result := f.d.Handle(ctx, message)
			if result.OK || f.count("pane.send_text") != 0 {
				t.Fatalf("fence: %+v", result)
			}
			if time.Since(started) > 600*time.Millisecond {
				t.Fatal("admission exceeded total budget")
			}
		})
	}
}
func TestScrollUncertainWritesAreNeverRetried(t *testing.T) {
	for _, mode := range []string{"drop", "late"} {
		t.Run(mode, func(t *testing.T) {
			f := newScrollFixture(t)
			message := f.message("scroll_agent", "uncertain", "owner")
			f.mu.Lock()
			if mode == "drop" {
				f.rawMode = "drop"
			} else {
				f.rawDelay = 300 * time.Millisecond
			}
			f.mu.Unlock()
			started := time.Now()
			result := f.d.Handle(context.Background(), message)
			if result.OK || result.Phase != "dispatched_unknown" || f.count("pane.send_text") != 1 {
				t.Fatalf("uncertain: %+v", result)
			}
			if time.Since(started) > 550*time.Millisecond {
				t.Fatal("late acknowledgement exceeded total budget")
			}
			if next := f.d.Handle(context.Background(), message); next.OK || next.Phase != "dispatched_unknown" || f.count("pane.send_text") != 1 {
				t.Fatalf("uncertain replay: %+v", next)
			}
		})
	}
}
func TestScrollBusyDoesNotBacklog(t *testing.T) {
	f := newScrollFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	f.mu.Lock()
	f.bridgeHook = func() { close(entered); <-release }
	f.mu.Unlock()
	first := f.message("scroll_agent", "first", "owner")
	result := make(chan *CommandResult, 1)
	go func() { result <- f.d.Handle(context.Background(), first) }()
	<-entered
	second := f.message("scroll_agent", "second", "owner")
	started := time.Now()
	busy := f.d.Handle(context.Background(), second)
	if busy.OK || time.Since(started) > 50*time.Millisecond {
		t.Fatalf("backlogged: %+v", busy)
	}
	release <- struct{}{}
	if completed := <-result; !completed.OK {
		t.Fatalf("first: %+v", completed)
	}
	if f.count("pane.send_text") != 1 {
		t.Fatal("busy intent delivered")
	}
}
func TestScrollReadCacheCannotJoinOldGeometry(t *testing.T) {
	f := newScrollFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	f.mu.Lock()
	f.readHook = func(count int) {
		if count == 1 {
			close(entered)
			<-release
		}
	}
	f.mu.Unlock()
	request := map[string]any{"action": "read_pane", "pane_id": "pane", "format": "ansi", "_scroll_owner": "owner"}
	results := make(chan map[string]any, 2)
	go func() { results <- f.d.HandleReadPane(context.Background(), request) }()
	<-entered
	f.d.InvalidateScrollGeometry("pane")
	f.mu.Lock()
	f.grid.Columns++
	f.mu.Unlock()
	target, _ := f.d.scrollTarget("pane")
	if err := f.d.scroll.Validate(context.Background(), target, true); err != nil {
		t.Fatal(err)
	}
	go func() { results <- f.d.HandleReadPane(context.Background(), request) }()
	release <- struct{}{}
	first, second := <-results, <-results
	if f.count("pane.read") != 2 {
		t.Fatalf("joined old read: %d", f.count("pane.read"))
	}
	failures := 0
	for _, result := range []map[string]any{first, second} {
		if result["error"] != nil {
			failures++
		}
	}
	if failures != 1 {
		t.Fatalf("old frame not rejected: %+v %+v", first, second)
	}
}
