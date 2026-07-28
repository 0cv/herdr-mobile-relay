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

func TestPaneSizeLeaseCommandResultIncludesAppliedColumns(t *testing.T) {
	got := commandResultMessage(&coordinator.CommandResult{
		RequestID: "req-size",
		Action:    "lease_pane_size",
		OK:        true,
		Phase:     "completed",
		PaneID:    "pane-1",
		Data:      map[string]any{"columns": 84},
	})
	if got["action"] != "lease_pane_size" ||
		!reflect.DeepEqual(got["data"], map[string]any{"columns": 84}) {
		t.Fatalf("pane size lease result = %#v", got)
	}
}

func TestPaneSizeLeaseMutationsRemainOrdered(t *testing.T) {
	for _, action := range []string{"lease_pane_size", "release_pane_size"} {
		if !isCoordinatorMutation(action) {
			t.Errorf("isCoordinatorMutation(%q) = false", action)
		}
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
