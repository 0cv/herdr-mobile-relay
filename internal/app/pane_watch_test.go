package app

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/panedelta"
	"github.com/0cv/herdr-mobile-relay/internal/question"
)

func TestPaneDeltaResponsePreservesTruncation(t *testing.T) {
	delta := paneDeltaResponse(
		map[string]any{
			"type":      "pane_content",
			"content":   "new output",
			"truncated": true,
			"format":    "ansi",
		},
		"content-1",
		nil,
	)
	if delta["truncated"] != true {
		t.Fatalf("delta truncation = %#v, want true", delta["truncated"])
	}
	if _, ok := delta["content"]; ok {
		t.Fatal("delta unexpectedly included full content")
	}
}

func TestPaneWatchUpdateSendsMetadataOnlyDelta(t *testing.T) {
	response := map[string]any{
		"type":           "pane_content",
		"content":        "unchanged\nquestion\n",
		"attention_kind": question.AttentionQuestion,
		"interaction":    map[string]any{"id": "question-1"},
	}
	previous := map[string]any{
		"type":           "pane_content",
		"content":        "unchanged\nquestion\n",
		"attention_kind": question.AttentionUnknown,
	}
	acknowledged := &paneWatchFrame{
		content:            "unchanged\nquestion\n",
		contentFingerprint: "content-1",
		frameFingerprint:   paneFrameFingerprint(previous),
	}
	current := &paneWatchFrame{
		content:            "unchanged\nquestion\n",
		contentFingerprint: "content-1",
		frameFingerprint:   paneFrameFingerprint(response),
	}
	if acknowledged.frameFingerprint == current.frameFingerprint {
		t.Fatal("question metadata did not change the pane frame fingerprint")
	}
	initial := *acknowledged
	initial.frameFingerprint = ""
	if paneWatchUpdate(response, &initial, current) == nil {
		t.Fatal("initial pane watch suppressed current interaction metadata")
	}

	update := paneWatchUpdate(response, acknowledged, current)
	if update["type"] != "pane_delta" ||
		update["base_fingerprint"] != "content-1" {
		t.Fatalf("metadata update = %#v", update)
	}
	if _, ok := update["content"]; ok {
		t.Fatal("metadata delta unexpectedly included full terminal content")
	}
	segments, ok := update["segments"].([]panedelta.Segment)
	if !ok || len(segments) != 1 || segments[0].CopyStart != 0 ||
		segments[0].CopyLines != 3 || segments[0].Text != "" {
		t.Fatalf("metadata delta segments = %#v, want one whole-frame copy", update["segments"])
	}
	applied, appliedOK := panedelta.Apply(acknowledged.content, segments)
	if !appliedOK || applied != current.content {
		t.Fatalf("metadata delta applied = %q, %v; want %q, true", applied, appliedOK, current.content)
	}
	encoded, err := json.Marshal(update)
	if err != nil {
		t.Fatalf("marshal metadata delta: %v", err)
	}
	if strings.Contains(string(encoded), `"segments":null`) {
		t.Fatalf("metadata delta encoded a null segment list: %s", encoded)
	}
}

func TestPaneWatchUpdateSendsResizeSettledDelta(t *testing.T) {
	settlingResponse := map[string]any{
		"type":            "pane_content",
		"content":         "unchanged terminal",
		"resize_settling": true,
	}
	settledResponse := map[string]any{
		"type":    "pane_content",
		"content": "unchanged terminal",
	}
	acknowledged := &paneWatchFrame{
		content:            "unchanged terminal",
		contentFingerprint: "content-1",
		frameFingerprint:   paneFrameFingerprint(settlingResponse),
		resizeSettling:     true,
	}
	current := &paneWatchFrame{
		content:            "unchanged terminal",
		contentFingerprint: "content-1",
		frameFingerprint:   paneFrameFingerprint(settledResponse),
	}
	if acknowledged.frameFingerprint == current.frameFingerprint {
		t.Fatal("resize settling state did not change the pane frame fingerprint")
	}

	if !paneWatchNeedsFrameRead("probe-1", "probe-1", acknowledged, "") {
		t.Fatal("settling pane frame would not be re-read after an unchanged probe")
	}
	if paneWatchNeedsFrameRead("probe-1", "probe-1", current, "") {
		t.Fatal("settled pane frame would be re-read after an unchanged probe")
	}
	update := paneWatchUpdate(settledResponse, acknowledged, current)
	if update["type"] != "pane_delta" ||
		update["base_fingerprint"] != "content-1" {
		t.Fatalf("resize settled update = %#v", update)
	}
	if update["resize_settling"] == true {
		t.Fatalf("resize settled update remained transient: %#v", update)
	}
}

func TestRequestedPaneWatchInterval(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  time.Duration
	}{
		{name: "fast", value: float64(100), want: 100 * time.Millisecond},
		{name: "default", value: float64(250), want: 250 * time.Millisecond},
		{name: "balanced battery", value: float64(500), want: 500 * time.Millisecond},
		{name: "slow", value: float64(1_000), want: time.Second},
		{name: "missing", value: nil, want: defaultPaneWatchInterval},
		{name: "unsupported", value: float64(333), want: defaultPaneWatchInterval},
		{name: "fractional", value: 100.5, want: defaultPaneWatchInterval},
		{name: "wrong type", value: "100", want: defaultPaneWatchInterval},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := requestedPaneWatchInterval(test.value); got != test.want {
				t.Fatalf("requestedPaneWatchInterval(%v) = %s, want %s", test.value, got, test.want)
			}
		})
	}
}
