package hc

import (
	"context"
	"errors"
	"net"
	"reflect"
	"regexp"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func TestLaunchParameters(t *testing.T) {
	seen := map[string]bool{}
	c := New(fakeCore(t, 1, func(conn net.Conn, req fakeRequest) {
		if req.Method == "doc.get" {
			reply(conn, req.ID, "result", map[string]any{"value": nil, "version": 0})
			return
		}
		labels := req.Params["labels"].(map[string]any)
		for _, key := range []string{"herdr.space", "herdr.tab"} {
			id := labels[key].(string)
			if !regexp.MustCompile("^(sp|tb)[0-9a-f]{32}$").MatchString(id) || seen[id] {
				t.Errorf("invalid or reused ID: %s", id)
			}
			seen[id] = true
		}
		want := map[string]any{"argv": []any{"/bin/sleep", "120", "-p", "default"}, "cwd": "/tmp", "name": "fixture", "labels": labels, "keep": true, "no_parent": true, "wait": false}
		if req.Method != "terminal.run" || !reflect.DeepEqual(req.Params, want) || labels["herdr.seen"] != "0" || len(labels) != 3 {
			t.Errorf("request: %+v", req)
		}
		reply(conn, req.ID, "result", map[string]any{"address": "t1", "pid": 123, "state": "live", "labels": labels})
	}))
	for _, workspaceID := range []string{"", "", "core", "remote/space"} {
		pane, _, err := c.LaunchTerminal(t.Context(), []string{"/bin/sleep", "120", "-p", "default"}, "/tmp", "fixture", workspaceID, "", false)
		if err != nil || pane == nil {
			t.Fatalf("launch: %v %v", pane, err)
		}
	}
}

func TestLaunchReadiness(t *testing.T) {
	for _, scenario := range []string{"idle", "busy", "unreported", "exited", "incarnation"} {
		t.Run(scenario, func(t *testing.T) {
			c := New(fakeCore(t, 1, func(conn net.Conn, req fakeRequest) {
				result := map[string]any{"address": "t1", "pid": 123, "state": "live", "labels": map[string]string{"herdr.space": "existing"}}
				switch req.Method {
				case "doc.get":
					reply(conn, req.ID, "result", map[string]any{"value": nil, "version": 0})
					return
				case "terminal.run":
					if req.Params["labels"].(map[string]any)["herdr.space"] != "existing" {
						t.Error("workspace not reused")
					}
				case "terminal.get":
					result["reported"] = scenario != "unreported"
					result["status"] = map[string]string{"state": "idle", "app": "fixture"}
					if scenario == "busy" {
						result["status"] = map[string]string{"state": "busy"}
					}
					if scenario == "exited" {
						result["state"] = "finished"
					}
					if scenario == "incarnation" {
						result["pid"] = 456
					}
				default:
					t.Errorf("unexpected operation %s", req.Method)
				}
				reply(conn, req.ID, "result", result)
			}))
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			pane, ready, err := c.LaunchTerminal(ctx, []string{"/bin/sleep", "120"}, "/tmp", "fixture", "existing", "", true)
			if err != nil || pane == nil || ready != (scenario == "idle") {
				t.Fatalf("launch = %v, %v, %v", pane, ready, err)
			}
		})
	}
}

func TestLaunchUnknownDoesNotRetry(t *testing.T) {
	var calls atomic.Int32
	c := New(fakeCore(t, 1, func(conn net.Conn, req fakeRequest) { calls.Add(1); conn.Close() }))
	_, _, err := c.LaunchTerminal(t.Context(), []string{"/bin/sleep", "120"}, "/tmp", "fixture", "", "", false)
	if !errors.Is(err, herdr.ErrDispatchedUnknown) || calls.Load() != 1 {
		t.Fatalf("calls=%d error=%v", calls.Load(), err)
	}
}
