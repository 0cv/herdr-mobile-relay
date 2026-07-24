package coordinator

// Regression tests for the coordinator's per-pane FIFO serialization.
// Helpers writeScript/testLogger live in sibling *_test.go files.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

// intervalScript writes "start"/"end" around a sleep so a test can detect
// whether two herdr invocations overlapped in time.
func intervalScript(t *testing.T, dir, log string) string {
	return writeScript(t, dir, "herdr", "#!/bin/sh\n"+
		"echo start >> \""+log+"\"\n"+
		"sleep 0.3\n"+
		"echo end >> \""+log+"\"\n"+
		"printf '{\"ok\":true}\\n'\n")
}

// peakConcurrency parses the start/end log and returns the peak number of
// simultaneously-active invocations.
func peakConcurrency(log string) int {
	data, _ := os.ReadFile(log)
	active, peak := 0, 0
	for _, tok := range strings.Fields(string(data)) {
		switch tok {
		case "start":
			active++
			if active > peak {
				peak = active
			}
		case "end":
			active--
		}
	}
	return peak
}

func runConcurrent(d *Dispatcher, a, b map[string]any) {
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); <-start; d.Handle(context.Background(), a) }()
	go func() { defer wg.Done(); <-start; d.Handle(context.Background(), b) }()
	close(start)
	wg.Wait()
}

// §10.2: two mutations targeting the SAME pane must be serialized through that
// pane's FIFO — never dispatched to the terminal concurrently.
func TestSamePaneCommandsSerialize(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "intervals.log")
	d := NewDispatcher(herdr.NewClient(intervalScript(t, dir, log), filepath.Join(dir, "sock")), NewState(testLogger()), nil, testLogger())

	runConcurrent(d,
		map[string]any{"action": "submit_prompt", "request_id": "a", "pane_id": "pane-1", "text": "one"},
		map[string]any{"action": "send_keys", "request_id": "b", "pane_id": "pane-1", "keys": []any{"x"}},
	)

	if peak := peakConcurrency(log); peak > 1 {
		t.Fatalf("same-pane commands ran %d concurrently, want serialized (peak 1) "+
			"(no per-pane FIFO/PaneSlot; concurrent input to one terminal)", peak)
	}
}

// §10.2 / §16.10: Clear must serialize with other mutations on the same pane —
// a prompt must never be dispatched concurrently with a clear of that pane.
func TestClearSerializesWithPrompt(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "intervals.log")
	d := NewDispatcher(herdr.NewClient(intervalScript(t, dir, log), filepath.Join(dir, "sock")), NewState(testLogger()), nil, testLogger())

	runConcurrent(d,
		map[string]any{"action": "agent_clear", "request_id": "a", "pane_id": "pane-1"},
		map[string]any{"action": "submit_prompt", "request_id": "b", "pane_id": "pane-1", "text": "one"},
	)

	if peak := peakConcurrency(log); peak > 1 {
		t.Fatalf("clear and prompt on one pane ran %d concurrently, want serialized (peak 1) "+
			"(no lifecycle serialization; a prompt can land on a pane being cleared)", peak)
	}
}
