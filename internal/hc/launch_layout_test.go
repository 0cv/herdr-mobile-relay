package hc

import (
	"context"
	"encoding/json"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func TestLaunchNamesAdoptedTab(t *testing.T) {
	for _, scenario := range []string{"adopted", "conflict", "absent"} {
		t.Run(scenario, func(t *testing.T) {
			var reads, writes atomic.Int32
			named := make(chan struct{}, 1)
			var tabID string
			c := New(fakeCore(t, 1, func(conn net.Conn, req fakeRequest) {
				switch req.Method {
				case "terminal.run":
					labels := req.Params["labels"].(map[string]any)
					tabID = labels["herdr.tab"].(string)
					reply(conn, req.ID, "result", map[string]any{"address": "t1", "pid": 123, "state": "live", "labels": labels})
				case "doc.get":
					n := reads.Add(1)
					var value any
					if scenario != "absent" && n > 1 {
						value = map[string]any{"future": json.RawMessage(`9007199254740993`), "spaces": []any{map[string]any{"id": "s", "tabs": []any{map[string]any{"id": tabID, "name": "", "root": map[string]any{"type": "block", "content": map[string]string{"terminal": "t1"}}}}}}}
					}
					reply(conn, req.ID, "result", map[string]any{"version": n, "value": value})
				case "doc.set":
					n := writes.Add(1)
					if scenario == "conflict" && n == 1 {
						reply(conn, req.ID, "error", map[string]any{"code": -32000, "data": map[string]string{"kind": "conflict"}})
						return
					}
					value := req.Params["value"].(map[string]any)
					tab := value["spaces"].([]any)[0].(map[string]any)["tabs"].([]any)[0].(map[string]any)
					if tab["name"] != "requested" || req.Params["version"] != float64(reads.Load()) {
						t.Errorf("rename did not re-read adoption: %+v", req.Params)
					}
					var wire struct {
						Params struct{ Value map[string]json.RawMessage }
					}
					json.Unmarshal(req.Raw, &wire)
					if string(wire.Params.Value["future"]) != "9007199254740993" {
						t.Error("unknown field lost precision")
					}
					reply(conn, req.ID, "result", map[string]any{"version": reads.Load() + 1})
					named <- struct{}{}
				default:
					t.Errorf("unexpected method %s", req.Method)
				}
			}))
			start := time.Now()
			pane, _, err := c.LaunchTerminal(t.Context(), []string{"/bin/sleep", "120"}, "/tmp", "requested", "s", "", false)
			if err != nil || pane == nil || time.Since(start) > time.Second {
				t.Fatalf("start waited for adoption: pane=%+v err=%v", pane, err)
			}
			if scenario == "absent" {
				time.Sleep(adoptionTimeout + 150*time.Millisecond)
				before := reads.Load()
				time.Sleep(100 * time.Millisecond)
				if before == 0 || reads.Load() != before || writes.Load() != 0 {
					t.Fatalf("absent app: reads=%d/%d writes=%d", before, reads.Load(), writes.Load())
				}
				return
			}
			select {
			case <-named:
			case <-time.After(3 * time.Second):
				t.Fatal("tab was not named")
			}
			want := int32(1)
			if scenario == "conflict" {
				want = 2
			}
			if writes.Load() != want {
				t.Fatalf("writes=%d want=%d", writes.Load(), want)
			}
		})
	}
}

func TestReplacementWaitsForAdoption(t *testing.T) {
	for _, adopted := range []bool{true, false} {
		t.Run(map[bool]string{true: "adopted", false: "absent"}[adopted], func(t *testing.T) {
			var reads atomic.Int32
			c := New(fakeCore(t, 1, func(conn net.Conn, req fakeRequest) {
				switch req.Method {
				case "terminal.run":
					labels := req.Params["labels"].(map[string]any)
					if labels["herdr.tab"] != "old-tab" || labels["herdr.space"] != "s" {
						t.Errorf("location=%v", labels)
					}
					reply(conn, req.ID, "result", map[string]any{"address": "t2", "pid": 124, "state": "live", "labels": labels})
				case "doc.get":
					terminal := "t1"
					if reads.Add(1) > 1 && adopted {
						terminal = "t2"
					}
					reply(conn, req.ID, "result", map[string]any{"version": 1, "value": map[string]any{"spaces": []any{map[string]any{"id": "s", "tabs": []any{map[string]any{"id": "old-tab", "name": "Keep me", "root": map[string]any{"type": "row", "children": []any{map[string]any{"type": "block", "content": map[string]string{"terminal": terminal}}}}}}}}}})
				default:
					t.Errorf("replacement must not rename layout: %s", req.Method)
				}
			}))
			ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
			defer cancel()
			pane, _, err := c.LaunchTerminal(ctx, []string{"/bin/sleep", "120"}, "/tmp", "replacement", "s", "old-tab", false)
			if pane == nil || pane.TabID != "old-tab" || err != nil {
				t.Fatalf("replacement=%+v err=%v", pane, err)
			}
		})
	}
}
