package coordinator

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/piscroll"
)

func TestScrollSelectionRechecksAuthorityAndCancellation(t *testing.T) {
	for _, cause := range []string{"revoked", "cancelled"} {
		t.Run(cause, func(t *testing.T) {
			f := newScrollFixture(t)
			message := f.message("set_scroll_selection", "fenced-selection", "owner")
			message["selection"] = "local"
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
			f.processHook = func(read int) {
				if read == 2 {
					if cause == "revoked" {
						revoked.Store(true)
					} else {
						cancel()
					}
				}
			}
			f.mu.Unlock()
			time.Sleep(510 * time.Millisecond)
			result := f.d.Handle(ctx, message)
			if result.OK || result.Phase != "not_started" || result.Action != "set_scroll_selection" {
				t.Fatalf("selection fence: %+v", result)
			}
			if state := f.d.ScrollState(context.Background(), "pane", "owner", false); state.Selection != "auto" {
				t.Fatal("unauthorized selection changed routing")
			}
			if f.count("pane.send_text") != 0 {
				t.Fatal("selection wrote terminal input")
			}
		})
	}
}
func TestScrollObservesRendererReplacementAfterGeometry(t *testing.T) {
	for _, change := range []string{"regular", "incarnation"} {
		t.Run(change, func(t *testing.T) {
			f := newScrollFixture(t)
			message := f.message("scroll_agent", "during-geometry", "owner")
			f.mu.Lock()
			f.geometryHook = func() {
				f.mu.Lock()
				defer f.mu.Unlock()
				if change == "regular" {
					f.renderer = "regular"
				} else {
					f.incarnation = "replacement"
				}
			}
			f.mu.Unlock()
			result := f.d.Handle(context.Background(), message)
			if result.OK || result.Phase != "not_started" || f.count("pane.send_text") != 0 {
				t.Fatalf("geometry-time renderer replacement wrote input: %+v", result)
			}
			if f.count("pane.process_info") != 2 || f.bridgeCalls.Load() != 1 || f.geometryCalls.Load() != 1 {
				t.Fatal("renderer validation expanded the preflight query budget")
			}
		})
	}
}

func TestScrollPostWriteReplacementKeepsUncertainLedger(t *testing.T) {
	f := newScrollFixture(t)
	target, _ := f.d.scrollTarget("pane")
	message := f.message("scroll_agent", "post-write", "owner")
	payload := struct {
		Target              piscroll.Target
		Direction, Revision string
	}{target, "up", message["state_revision"].(string)}
	f.mu.Lock()
	f.rawHook = func() {
		f.d.state.CommitInventory([]*AgentState{{PaneID: "pane", TerminalID: "terminal", SessionID: "replacement", Agent: "pi", Status: "idle"}}, f.d.state.Revision("pane"))
	}
	f.mu.Unlock()
	result := f.d.Handle(context.Background(), message)
	if result.OK || result.Phase != "dispatched_unknown" || f.count("pane.send_text") != 1 {
		t.Fatalf("post-write replacement: %+v", result)
	}
	replay, found, err := f.d.scheduler.ReplayLedger("scroll\x00owner\x00pane\x00post-write", hashPayload(payload))
	if err != nil || !found || replay == nil || replay.Phase != "dispatched_unknown" {
		t.Fatalf("uncertainty erased: %+v %v", replay, err)
	}
	if retry := f.d.Handle(context.Background(), message); retry.OK || f.count("pane.send_text") != 1 {
		t.Fatalf("uncertain request retried: %+v", retry)
	}
}
