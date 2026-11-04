package hc

import (
	"context"
	"errors"
	"fmt"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"net"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPaneReads(t *testing.T) {
	for _, complete := range []bool{true, false} {
		for _, lines := range []int{-1, 0, 80, 200, 10000, 10001} {
			for _, method := range []string{"read", "recent", "visible", "probe"} {
				t.Run(fmt.Sprintf("%s/%d/complete=%v", method, lines, complete), func(t *testing.T) {
					screen := strings.Repeat("screen row\n", 120)
					wantLines := min(max(lines, 0), 10000)
					if method == "visible" || method == "probe" {
						wantLines = 0
					}
					c := New(fakeCore(t, 1, func(conn net.Conn, req fakeRequest) {
						if req.Method != "terminal.read" || !reflect.DeepEqual(req.Params, map[string]any{"terminal": "term", "lines": float64(wantLines)}) {
							t.Errorf("request = %+v", req)
						}
						reply(conn, req.ID, "result", map[string]any{"text": screen, "complete": complete, "how": "screen", "why": ""})
					}))
					read := c.ReadPane
					switch method {
					case "recent":
						read = c.ReadPaneRecent
					case "visible":
						read = c.ReadPaneVisible
					case "probe":
						got, err := c.ProbePaneVisible(t.Context(), "term", lines, "text")
						if err != nil || string(got) != screen {
							t.Fatalf("probe = %q, %v", got, err)
						}
						_, err = c.ProbePaneVisible(t.Context(), "remote/term", lines, "text")
						if !herdr.IsPaneNotFound(err) {
							t.Fatal(err)
						}
						return
					}
					got, err := read(t.Context(), "term", lines, "ansi")
					if err != nil || string(got.Content) != screen || got.Truncated != !complete {
						t.Fatalf("read = %+v, %v", got, err)
					}
					_, err = read(t.Context(), "remote/term", lines, "text")
					if !herdr.IsPaneNotFound(err) {
						t.Fatalf("remote read = %v", err)
					}
				})
			}
		}
	}
}

func TestOperations(t *testing.T) {
	for _, tc := range []struct {
		name, method string
		params       map[string]any
		run          func(*Client) error
	}{
		{"prompt", "terminal.send", map[string]any{"text": "answer", "force": true}, func(c *Client) error { return c.Prompt(t.Context(), "term", "answer") }},
		{"keys", "terminal.keys", map[string]any{"keys": []string{"enter", "shift+tab", "down", "f5", "ctrl+c", "esc", "shift+x", "up", "left", "right", "tab", "alt+x", "ctrl+shift+a", "space", "ctrl+u"}}, func(c *Client) error {
			return c.SendKeys(t.Context(), "term", []string{"Enter", "Shift+Tab", "Down", "f5", "ctrl+c", "Escape", "X", "Up", "Left", "Right", "Tab", "alt+x", "ctrl+shift+a", "Space", "Ctrl+U"})
		}},
		{"literal uppercase", "terminal.keys", map[string]any{"keys": []string{"shift+x"}}, func(c *Client) error { return c.SendLiteralKey(t.Context(), "term", "X") }},
		{"literal space", "terminal.keys", map[string]any{"keys": []string{"space"}}, func(c *Client) error { return c.SendLiteralKey(t.Context(), "term", " ") }},
		{"literal unchanged", "terminal.keys", map[string]any{"keys": []string{"é"}}, func(c *Client) error { return c.SendLiteralKey(t.Context(), "term", "é") }},
		{"stop", "terminal.kill", map[string]any{}, func(c *Client) error { return c.StopPane(t.Context(), "term") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			c := New(fakeCore(t, 1, func(conn net.Conn, req fakeRequest) {
				calls.Add(1)
				want := map[string]any{"terminal": "term"}
				for k, v := range tc.params {
					if keys, ok := v.([]string); ok {
						a := make([]any, len(keys))
						for i, k := range keys {
							a[i] = k
						}
						v = a
					}
					want[k] = v
				}
				if req.Method != tc.method || !reflect.DeepEqual(req.Params, want) {
					t.Errorf("request = %+v, want %s %+v", req, tc.method, want)
				}
				if req.Method == "terminal.send" && req.Params["force"] != true {
					reply(conn, req.ID, "error", map[string]any{"code": -32000, "message": "Terminal is blocked", "data": map[string]any{"kind": "blocked"}})
					return
				}
				reply(conn, req.ID, "result", map[string]any{"status": "blocked", "turn": 0})
			}))
			if err := tc.run(c); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 1 {
				t.Fatalf("calls = %d", calls.Load())
			}
		})
	}
}

func TestOperationRefusals(t *testing.T) {
	for _, method := range []string{"terminal.send", "terminal.keys"} {
		c := New(fakeCore(t, 1, func(conn net.Conn, req fakeRequest) {
			if req.Method != method {
				t.Errorf("method = %s", req.Method)
			}
			kind := "blocked"
			if method == "terminal.keys" {
				kind = "invalid_params"
			}
			reply(conn, req.ID, "error", map[string]any{"code": -32602, "message": "refused", "data": map[string]any{"kind": kind}})
		}))
		var err error
		if method == "terminal.send" {
			err = c.Prompt(t.Context(), "term", "answer")
		} else {
			err = c.SendKeys(t.Context(), "term", []string{"unknown-key"})
		}
		if !herdr.IsRefused(err) || errors.Is(err, herdr.ErrDispatchedUnknown) {
			t.Fatalf("error = %v", err)
		}
	}
}

func TestCapabilitiesAndLifecycle(t *testing.T) {
	c := New(fakeCore(t, 1, func(conn net.Conn, req fakeRequest) { t.Errorf("unexpected request: %+v", req) }))
	status := c.CapabilityStatus()
	if len(status.Features) != 3 || !status.Supports(herdr.FeaturePaneRead) || !status.Supports(herdr.FeatureSecretInput) || !status.Supports(herdr.FeatureManualTerminalInput) {
		t.Fatalf("status = %+v", status)
	}
	c.SetCapabilityChangeCallback(func(herdr.ServerStatus) { t.Error("unexpected callback") })
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { c.RunCapabilityRefresh(ctx, time.Nanosecond); close(done) }()
	select {
	case <-done:
		t.Fatal("refresh returned before cancellation")
	case <-time.After(10 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("refresh did not stop")
	}
	got, err := c.IntegrationStatus(t.Context())
	if err != nil || string(got) != "{}" {
		t.Fatalf("integration = %q, %v", got, err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}
