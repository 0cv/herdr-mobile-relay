package coordinator

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func filterScreen(value string) string {
	return " Available models    Max mode: OFF\n\n Filter: " + strings.TrimRight(value, " ") + "\n\n → Grok\n\n Type to filter • Enter to select • Tab to edit"
}

func TestCursorFilterLiveFixtures(t *testing.T) {
	data, err := os.ReadFile("testdata/cursor_filter_live.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Frames []struct {
			Screen string `json:"screen"`
		} `json:"frames"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Frames) == 0 {
		t.Fatal("missing live frames")
	}
	for _, frame := range fixture.Frames {
		value, err := cursorFilterValue(frame.Screen)
		if err != nil {
			t.Fatalf("live screen refused: %v\n%s", err, frame.Screen)
		}
		if !strings.Contains(frame.Screen, "Filter: "+value) && value != "" {
			t.Fatalf("incorrect field: %q", value)
		}
		if _, err := cursorFilterValue(frame.Screen + "\nSelected model\n>"); err == nil {
			t.Fatal("accepted stale live footer")
		}
	}
}

func TestCursorFilterValue(t *testing.T) {
	for _, value := range []string{"", "grok", "GrOk22.-_/", "g r", "  g"} {
		screen := filterScreen(value)
		for _, frame := range []string{screen, strings.ReplaceAll(screen, "select • Tab", "select •\n Tab")} {
			got, err := cursorFilterValue(frame)
			if err != nil || got != value {
				t.Fatalf("value %q: %q, %v", value, got, err)
			}
		}
	}
	for _, frame := range []string{filterScreen("g") + "\n›", "Filter: g\nType to filter • Enter to select • Tab to edit", strings.ReplaceAll(filterScreen("g"), "Max mode:", "Other:")} {
		if _, err := cursorFilterValue(frame); err == nil {
			t.Fatalf("accepted stale or unsupported screen %q", frame)
		}
	}
}

func TestFilterDelivery(t *testing.T) {
	for _, tc := range []struct{ name, initial, text, mode, phase, want string }{
		{"accepted", "", "GrOk22", "", "completed", "GrOk22"},
		{"spaces", "g", " r", "", "completed", "g r"},
		{"repeated", "g", "oo", "", "completed", "goo"},
		{"hidden starting space", "g ", "rok", "", "dispatched_unknown", "g r"},
		{"ignored", "", "grok", "ignore", "dispatched_unknown", ""},
		{"closed", "", "grok", "close", "dispatched_unknown", "g"},
		{"lost acknowledgement", "", "grok", "lost", "dispatched_unknown", "g"},
		{"replacement", "", "grok", "replace", "dispatched_unknown", "g"},
		{"cancel after prefix", "", "grok", "cancel", "dispatched_unknown", "g"},
		{"trailing space", "", "g ", "", "failed", ""},
		{"newline", "", "g\n", "", "failed", ""},
		{"unicode", "", "g😀", "", "failed", ""},
		{"long", "", strings.Repeat("g", 33), "", "failed", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newFakeBackend()
			var mu sync.Mutex
			value := tc.initial
			writes := 0
			var times []time.Time
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			state := NewState(testLogger())
			state.CommitInventory([]*AgentState{{PaneID: "pane-1", Agent: "cursor", Status: "working"}}, state.RevisionCounter())
			client.setHook("ReadPaneVisible", func(context.Context, fakeCall) error {
				mu.Lock()
				defer mu.Unlock()
				screen := filterScreen(value)
				if tc.mode == "close" && writes > 0 {
					screen += "\n›"
				}
				client.update(func(b *fakeBackend) { b.contents["pane-1"] = screen })
				return nil
			})
			client.setHook("SendLiteralKey", func(_ context.Context, call fakeCall) error {
				mu.Lock()
				defer mu.Unlock()
				writes++
				if tc.mode == "replace" {
					state.BumpGeneration("pane-1")
				}
				if tc.mode == "cancel" {
					cancel()
				}
				times = append(times, time.Now())
				if tc.mode != "ignore" {
					value += call.Args[1].(string)
				}
				if tc.mode == "lost" {
					return &herdr.OutcomeError{Started: true, Err: io.EOF}
				}
				return nil
			})
			d := newTestDispatcher(t, client, state)
			result := d.Handle(ctx, map[string]any{"type": "send_filter_text", "pane_id": "pane-1", "text": tc.text})
			if result.Phase != tc.phase {
				t.Errorf("result = %+v, want phase %s", result, tc.phase)
			}
			mu.Lock()
			defer mu.Unlock()
			if value != tc.want {
				t.Errorf("value = %q, want %q", value, tc.want)
			}
			if !result.OK && writes > 1 {
				t.Errorf("continued after uncertainty: %d writes", writes)
			}
			for i := 1; i < len(times); i++ {
				if times[i].Sub(times[i-1]) < 150*time.Millisecond {
					t.Error("writes not paced")
				}
			}
		})
	}
}
