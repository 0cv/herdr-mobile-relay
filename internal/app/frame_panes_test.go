package app

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
)

func newPaneFrameEnv(t *testing.T) *frameEnv {
	t.Helper()
	return newFrameEnv(t, &coordinator.AgentState{
		PaneID: "pane-1", Agent: "codex", TerminalID: "terminal-1", TabID: "tab-1", Generation: 1,
		SessionID: "agent-session-1", ServerSessionID: protocol.PrimaryServerSessionID, Status: "working",
	})
}

func TestFrameReadPaneLines(t *testing.T) {
	for name, test := range map[string]struct {
		lines any
		read  string
	}{
		"absent":        {nil, "ReadPaneVisible pane-1 lines=30 format=text"},
		"zero":          {0, "ReadPaneVisible pane-1 lines=1 format=text"},
		"negative":      {-5, "ReadPaneVisible pane-1 lines=1 format=text"},
		"one":           {1, "ReadPaneVisible pane-1 lines=1 format=text"},
		"inside":        {120, "ReadPaneVisible pane-1 lines=120 format=text"},
		"the maximum":   {10000, "ReadPaneVisible pane-1 lines=10000 format=text"},
		"above the max": {20000, "ReadPaneVisible pane-1 lines=10000 format=text"},
		"far above":     {1 << 40, "ReadPaneVisible pane-1 lines=10000 format=text"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newPaneFrameEnv(t)
			fields := map[string]any{"type": "read_pane"}
			if test.lines != nil {
				fields["lines"] = test.lines
			}
			if response := e.exchange(t, fields); response["type"] != "pane_content" || response["content"] != "ready\n" {
				t.Fatalf("response = %#v", response)
			}
			if got := e.backend.called("Read"); !slices.Equal(got, []string{test.read}) {
				t.Fatalf("reads = %v, want %s", got, test.read)
			}
		})
	}
}

func TestFrameReadPaneFormat(t *testing.T) {
	for format, read := range map[string]string{
		"ansi":    "ReadPaneRecent pane-1 lines=30 format=ansi",
		"text":    "ReadPaneVisible pane-1 lines=30 format=text",
		"other":   "ReadPaneVisible pane-1 lines=30 format=text",
		"":        "ReadPaneVisible pane-1 lines=30 format=text",
		"unknown": "ReadPaneVisible pane-1 lines=30 format=text",
	} {
		t.Run(format, func(t *testing.T) {
			e := newPaneFrameEnv(t)
			e.exchange(t, map[string]any{"type": "read_pane", "format": format})
			if got := e.backend.called("Read"); !slices.Equal(got, []string{read}) {
				t.Fatalf("reads = %v, want %s", got, read)
			}
		})
	}
}

func TestFrameReadPaneIgnoresClientGeometryWithoutALease(t *testing.T) {
	e := newPaneFrameEnv(t)
	newPaneSizeFakes(e.server)
	response := e.exchange(t, map[string]any{"type": "read_pane", "terminal_columns": 50, "terminal_rows": 12})
	if response["type"] != "pane_content" || response["viewport_only"] != false {
		t.Fatalf("response = %#v, want a full read", response)
	}
	if _, present := response["viewport_rows"]; present {
		t.Fatalf("response = %#v, want no client-chosen viewport rows", response)
	}
}

func TestFrameReadPaneFollowsTheReadersOwnLease(t *testing.T) {
	owner := newPaneFrameEnv(t)
	newPaneSizeFakes(owner.server)
	expectCommandResult(t, owner.exchange(t, map[string]any{"type": "lease_pane_size", "columns": 80, "opt_in": true}), "lease_pane_size", true, "completed", "")
	other := dialHub(t, owner.server)

	geometry := map[string]any{"type": "read_pane", "terminal_columns": 50, "terminal_rows": 12}
	leased := owner.exchange(t, geometry)
	if leased["viewport_only"] != true || leased["viewport_rows"] != float64(40) {
		t.Fatalf("lease holder read = %#v, want the leased viewport at 40 rows", leased)
	}
	sendFrame(t, other, frameWithDefaults(geometry))
	unleased := readFrame(t, other)
	if unleased["viewport_only"] != false {
		t.Fatalf("a client without a lease read = %#v, want a full read", unleased)
	}
	if _, present := unleased["viewport_rows"]; present {
		t.Fatalf("a client without a lease read = %#v, want no viewport rows", unleased)
	}
}

func TestFrameReadPaneRepliesUnchangedForTheKnownFingerprint(t *testing.T) {
	e := newPaneFrameEnv(t)
	target := e.target(t, "pane-1")
	first := e.exchange(t, map[string]any{"type": "read_pane", "target": target})
	fingerprint, _ := first["content_fingerprint"].(string)
	if first["type"] != "pane_content" || fingerprint == "" {
		t.Fatalf("first read = %#v", first)
	}

	unchanged := e.exchange(t, map[string]any{"type": "read_pane", "target": target, "content_fingerprint": fingerprint})
	if unchanged["type"] != "pane_unchanged" || unchanged["request_id"] != "req-1" || unchanged["pane_id"] != "pane-1" ||
		unchanged["content_fingerprint"] != fingerprint || fmt.Sprint(unchanged["target"]) != fmt.Sprint(target) {
		t.Fatalf("unchanged reply = %#v", unchanged)
	}
	if _, present := unchanged["content"]; present {
		t.Fatalf("unchanged reply carries content: %#v", unchanged)
	}

	for name, known := range map[string]string{"another fingerprint": "0000000000000000", "empty fingerprint": ""} {
		full := e.exchange(t, map[string]any{"type": "read_pane", "target": target, "content_fingerprint": known})
		if full["type"] != "pane_content" || full["content"] != "ready\n" || full["content_fingerprint"] != fingerprint {
			t.Fatalf("%s: reply = %#v, want the full content", name, full)
		}
	}
}

func TestFrameWatchPaneIntervals(t *testing.T) {
	for name, test := range map[string]struct {
		interval any
		period   time.Duration
	}{
		"100 ms":      {100, 100 * time.Millisecond},
		"250 ms":      {250, 250 * time.Millisecond},
		"500 ms":      {500, 500 * time.Millisecond},
		"1000 ms":     {1000, time.Second},
		"absent":      {nil, 250 * time.Millisecond},
		"unsupported": {333, 250 * time.Millisecond},
		"zero":        {0, 250 * time.Millisecond},
		"negative":    {-100, 250 * time.Millisecond},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newPaneFrameEnv(t)
			fields := map[string]any{"type": "watch_pane"}
			if test.interval != nil {
				fields["interval_ms"] = test.interval
			}
			sendFrame(t, e.conn, frameWithDefaults(fields))
			first := readFrame(t, e.conn)
			fingerprint, _ := first["content_fingerprint"].(string)
			if first["type"] != "pane_content" || first["ack_required"] != true || fingerprint == "" {
				t.Fatalf("first watch frame = %#v", first)
			}
			sendFrame(t, e.conn, frameWithDefaults(map[string]any{"type": "pane_applied", "content_fingerprint": fingerprint}))

			const probes = 4
			deadline := time.Now().Add(time.Duration(probes+3) * test.period * 2)
			for len(e.backend.probeTimes()) < probes {
				if time.Now().After(deadline) {
					t.Fatalf("saw %d probes, want %d", len(e.backend.probeTimes()), probes)
				}
				time.Sleep(10 * time.Millisecond)
			}
			times := e.backend.probeTimes()
			average := times[probes-1].Sub(times[0]) / (probes - 1)
			if average < test.period*8/10 || average > test.period*16/10 {
				t.Fatalf("average time between probes = %s, want about %s", average, test.period)
			}
		})
	}
}

func TestFrameWatchPaneLines(t *testing.T) {
	for name, test := range map[string]struct {
		lines any
		read  string
	}{
		"absent": {nil, "ReadPaneVisible pane-1 lines=30 format=text"},
		"zero":   {0, "ReadPaneVisible pane-1 lines=1 format=text"},
		"inside": {200, "ReadPaneVisible pane-1 lines=200 format=text"},
		"above":  {20000, "ReadPaneVisible pane-1 lines=10000 format=text"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newPaneFrameEnv(t)
			fields := map[string]any{"type": "watch_pane"}
			if test.lines != nil {
				fields["lines"] = test.lines
			}
			sendFrame(t, e.conn, frameWithDefaults(fields))
			readFrame(t, e.conn)
			if got := e.backend.called("Read"); len(got) == 0 || got[0] != test.read {
				t.Fatalf("reads = %v, want first %s", got, test.read)
			}
		})
	}
}

func TestFrameWatchPaneStartsFromAKnownFingerprint(t *testing.T) {
	e := newPaneFrameEnv(t)
	read := e.exchange(t, map[string]any{"type": "read_pane"})
	fingerprint, _ := read["content_fingerprint"].(string)

	sendFrame(t, e.conn, frameWithDefaults(map[string]any{"type": "watch_pane", "content_fingerprint": fingerprint}))
	delta := readFrame(t, e.conn)
	if delta["type"] != "pane_delta" || delta["base_fingerprint"] != fingerprint || delta["content_fingerprint"] != fingerprint {
		t.Fatalf("watch from a known fingerprint = %#v, want a delta on it", delta)
	}
	if _, present := delta["content"]; present {
		t.Fatalf("delta carries content: %#v", delta)
	}

	sendFrame(t, e.conn, frameWithDefaults(map[string]any{"type": "watch_pane", "content_fingerprint": "0000000000000000"}))
	full := readFrame(t, e.conn)
	if full["type"] != "pane_content" || full["content"] != "ready\n" || full["ack_required"] != true {
		t.Fatalf("watch from an unknown fingerprint = %#v, want the full content", full)
	}
}

func TestFramePaneAppliedAcknowledgesOnlyTheSentFrame(t *testing.T) {
	e := newPaneFrameEnv(t)
	target := e.target(t, "pane-1")
	sendFrame(t, e.conn, frameWithDefaults(map[string]any{"type": "watch_pane", "target": target}))
	first := readFrame(t, e.conn)
	fingerprint, _ := first["content_fingerprint"].(string)
	if fingerprint == "" {
		t.Fatalf("first watch frame = %#v", first)
	}

	resync := e.exchange(t, map[string]any{"type": "pane_applied", "content_fingerprint": "0000000000000000"})
	if resync["type"] != "pane_resync" || resync["pane_id"] != "pane-1" || fmt.Sprint(resync["target"]) != fmt.Sprint(target) {
		t.Fatalf("unknown fingerprint reply = %#v, want a resync", resync)
	}
	for range 2 {
		sendFrame(t, e.conn, frameWithDefaults(map[string]any{"type": "pane_applied", "content_fingerprint": fingerprint}))
	}
	if replies := e.settle(t); len(replies) != 0 {
		t.Fatalf("the acknowledged fingerprint got replies: %v", replies)
	}

	if again := e.exchange(t, map[string]any{"type": "pane_applied", "content_fingerprint": "1111111111111111"}); again["type"] != "pane_resync" {
		t.Fatalf("unknown fingerprint after an acknowledged frame = %#v, want a resync", again)
	}
}
