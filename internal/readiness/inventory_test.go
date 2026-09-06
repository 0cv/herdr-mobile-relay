package readiness

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExactExpectedInventory(t *testing.T) {
	manifest := writeManifest(t, `{"version":1,"generation":"generation-7","panes":[{"pane_id":"pane-personal","native_session_id":"session-personal","profile_id":"personal"},{"pane_id":"pane-emu","native_session_id":"session-emu","profile_id":"emu"}]}`)
	observed := []Pane{
		{PaneID: "pane-emu", NativeSessionID: "session-emu", ProfileID: "emu"},
		{PaneID: "pane-personal", NativeSessionID: "session-personal", ProfileID: "personal"},
	}
	result := Check(manifest, "generation-7", observed)
	if !result.Ready || result.State != StateReady || result.Expected != 2 || result.Observed != 2 {
		t.Fatalf("exact inventory result = %+v", result)
	}
}

func TestAcknowledgedEmptyIsGenerationBoundAndDistinct(t *testing.T) {
	manifest := writeManifest(t, `{"version":1,"generation":"generation-empty","acknowledged_empty":true,"panes":[]}`)
	if result := Check(manifest, "generation-empty", nil); !result.Ready || result.State != StateAcknowledgedEmpty {
		t.Fatalf("acknowledged empty result = %+v", result)
	}
	if result := Check(manifest, "other-generation", nil); result.Ready || result.State != StateGenerationMismatch {
		t.Fatalf("cross-generation acknowledgement result = %+v", result)
	}
}

func TestExpectedInventoryFailsClosed(t *testing.T) {
	tests := []struct {
		name       string
		manifest   string
		generation string
		observed   []Pane
		want       State
	}{
		{name: "unexpected empty", manifest: `{"version":1,"generation":"g","panes":[{"pane_id":"p","native_session_id":"s","profile_id":"personal"}]}`, generation: "g", want: StateUnexpectedEmpty},
		{name: "partial", manifest: `{"version":1,"generation":"g","panes":[{"pane_id":"p1","native_session_id":"s1","profile_id":"personal"},{"pane_id":"p2","native_session_id":"s2","profile_id":"emu"}]}`, generation: "g", observed: []Pane{{PaneID: "p1", NativeSessionID: "s1", ProfileID: "personal"}}, want: StateInventoryMismatch},
		{name: "extra", manifest: `{"version":1,"generation":"g","panes":[]}`, generation: "g", observed: []Pane{{PaneID: "p", NativeSessionID: "s", ProfileID: "personal"}}, want: StateInventoryMismatch},
		{name: "wrong native session", manifest: `{"version":1,"generation":"g","panes":[{"pane_id":"p","native_session_id":"expected","profile_id":"personal"}]}`, generation: "g", observed: []Pane{{PaneID: "p", NativeSessionID: "other", ProfileID: "personal"}}, want: StateInventoryMismatch},
		{name: "wrong profile", manifest: `{"version":1,"generation":"g","panes":[{"pane_id":"p","native_session_id":"s","profile_id":"personal"}]}`, generation: "g", observed: []Pane{{PaneID: "p", NativeSessionID: "s", ProfileID: "emu"}}, want: StateInventoryMismatch},
		{name: "empty manifest not acknowledged", manifest: `{"version":1,"generation":"g","panes":[]}`, generation: "g", want: StateUnexpectedEmpty},
		{name: "ack with pane", manifest: `{"version":1,"generation":"g","acknowledged_empty":true,"panes":[{"pane_id":"p","native_session_id":"s","profile_id":"personal"}]}`, generation: "g", want: StateInvalidManifest},
		{name: "duplicate expected pane", manifest: `{"version":1,"generation":"g","panes":[{"pane_id":"p","native_session_id":"s1","profile_id":"personal"},{"pane_id":"p","native_session_id":"s2","profile_id":"emu"}]}`, generation: "g", want: StateInvalidManifest},
		{name: "duplicate observed session", manifest: `{"version":1,"generation":"g","panes":[{"pane_id":"p1","native_session_id":"s","profile_id":"personal"}]}`, generation: "g", observed: []Pane{{PaneID: "p1", NativeSessionID: "s", ProfileID: "personal"}, {PaneID: "p2", NativeSessionID: "s", ProfileID: "emu"}}, want: StateInvalidInventory},
		{name: "newer format", manifest: `{"version":2,"generation":"g","panes":[]}`, generation: "g", want: StateInvalidManifest},
		{name: "corrupt", manifest: `{`, generation: "g", want: StateInvalidManifest},
		{name: "unknown field", manifest: `{"version":1,"generation":"g","panes":[],"future":true}`, generation: "g", want: StateInvalidManifest},
		{name: "trailing value", manifest: `{"version":1,"generation":"g","panes":[]} {}`, generation: "g", want: StateInvalidManifest},
		{name: "trailing invalid", manifest: `{"version":1,"generation":"g","panes":[]} nope`, generation: "g", want: StateInvalidManifest},
		{name: "blank observed pane", manifest: `{"version":1,"generation":"g","panes":[{"pane_id":"p","native_session_id":"s","profile_id":"personal"}]}`, generation: "g", observed: []Pane{{PaneID: " ", NativeSessionID: "s", ProfileID: "personal"}}, want: StateInvalidInventory},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := Check(writeManifest(t, test.manifest), test.generation, test.observed)
			if result.Ready || result.State != test.want {
				t.Fatalf("result = %+v, want %s", result, test.want)
			}
		})
	}
}

func TestExpectedInventoryRejectsMissingAndPublicFiles(t *testing.T) {
	if result := Check(filepath.Join(t.TempDir(), "missing.json"), "g", nil); result.State != StateUnavailable {
		t.Fatalf("missing manifest result = %+v", result)
	}
	path := writeManifest(t, `{"version":1,"generation":"g","acknowledged_empty":true,"panes":[]}`)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if result := Check(path, "g", nil); result.State != StateInvalidManifest {
		t.Fatalf("public manifest result = %+v", result)
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	if result := Check(path, "g", nil); result.State != StateUnavailable {
		t.Fatalf("unreadable manifest result = %+v", result)
	}
}

func TestReadinessValidationHelpers(t *testing.T) {
	for _, value := range []string{"", strings.Repeat("x", 257), "value\n", "value\x7f"} {
		if validValue(value) {
			t.Fatalf("invalid value accepted: %q", value)
		}
	}
	if !validValue("value") {
		t.Fatal("valid value rejected")
	}

	for _, input := range []string{"{}", "nope"} {
		decoder := json.NewDecoder(bytes.NewBufferString(input))
		if err := trailingJSON(decoder); err == nil {
			t.Fatalf("trailing JSON %q accepted", input)
		}
	}
}

func writeManifest(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "expected-inventory.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
