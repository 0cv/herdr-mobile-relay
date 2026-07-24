package app

import (
	"reflect"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
)

func TestCommandResultMessageKeepsPythonMandatoryEmptyFields(t *testing.T) {
	got := commandResultMessage(&coordinator.CommandResult{
		RequestID: "req-001",
		Action:    "prompt",
		OK:        true,
		Phase:     "completed",
		PaneID:    "pane-1",
	})
	want := map[string]any{
		"type":       "command_result",
		"request_id": "req-001",
		"action":     "prompt",
		"ok":         true,
		"phase":      "completed",
		"error":      "",
		"pane_id":    "pane-1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("command result = %#v, want %#v", got, want)
	}
}

func TestCanonicalHTTPPathRejectsDotAndEmptySegments(t *testing.T) {
	for _, candidate := range []string{"/assets/../index.html", "/assets//app.js", "/assets/./app.js"} {
		if canonicalHTTPPath(candidate) {
			t.Errorf("canonicalHTTPPath(%q) = true", candidate)
		}
	}
	for _, candidate := range []string{"/", "/healthz", "/assets/app.js"} {
		if !canonicalHTTPPath(candidate) {
			t.Errorf("canonicalHTTPPath(%q) = false", candidate)
		}
	}
}
