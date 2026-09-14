package herdr

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCapabilityRefreshChecksPaneReadAfterReconnect(t *testing.T) {
	path, listener, done := capabilitySocket(t, []map[string]any{
		{"type": "pong", "version": "0.9.0", "protocol": 1},
		{"error": map[string]any{"code": "workspace_move_block_failed", "message": "empty selection"}},
		{"error": map[string]any{"code": "tab_not_found", "message": "empty tab"}},
		{"error": map[string]any{"code": "pane_not_found", "message": "empty pane"}},
		// A healthy refresh reuses evidence without probing a terminal again.
		{"type": "pong", "version": "0.9.0", "protocol": 1},
		{"error": map[string]any{"code": "workspace_move_block_failed", "message": "empty selection"}},
		// A same-version reconnect must recheck instead of retaining support.
		{"type": "pong", "version": "0.9.0", "protocol": 1},
		{"error": map[string]any{"code": "workspace_move_block_failed", "message": "empty selection"}},
		{"error": map[string]any{"code": "tab_not_found", "message": "empty tab"}},
		{"error": map[string]any{"code": "unknown_method", "message": "unsupported"}},
	})
	defer listener.Close()
	client := NewClient("missing-herdr", path)
	defer client.Close()

	// Event Bootstrap invalidates every live capability, including on startup.
	client.InvalidateLiveCapabilities()
	for range 2 {
		status := client.RefreshCapabilities(context.Background())
		feature := status.Feature(FeaturePaneRead)
		if feature.State != FeatureSupported || feature.Reason != "recognized_validation_refusal" {
			t.Fatalf("pane.read evidence = %+v, want a completed check without opening a terminal", feature)
		}
	}
	client.InvalidateLiveCapabilities()
	status := client.RefreshCapabilities(context.Background())
	feature := status.Feature(FeaturePaneRead)
	if feature.State != FeatureUnsupported || feature.Reason != "method_not_supported" {
		t.Fatalf("pane.read evidence after reconnect = %+v, want current server refusal", feature)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCapabilityRefreshRetriesUnknownPaneRead(t *testing.T) {
	path, listener, done := capabilitySocket(t, []map[string]any{
		{"type": "pong", "version": "0.9.0", "protocol": 1},
		{"error": map[string]any{"code": "workspace_move_block_failed", "message": "empty selection"}},
		{"error": map[string]any{"code": "tab_not_found", "message": "empty tab"}},
		{"error": map[string]any{"code": "invalid_request", "message": "temporary refusal"}},
		{"type": "pong", "version": "0.9.0", "protocol": 1},
		{"error": map[string]any{"code": "workspace_move_block_failed", "message": "empty selection"}},
		{"error": map[string]any{"code": "pane_not_found", "message": "empty pane"}},
	})
	defer listener.Close()
	client := NewClient("missing-herdr", path)
	defer client.Close()
	first := client.RefreshCapabilities(context.Background()).Feature(FeaturePaneRead)
	if first.State != FeatureUnknown || first.Reason != "probe_failed" {
		t.Fatalf("first check = %+v, want unknown/probe_failed", first)
	}
	second := client.RefreshCapabilities(context.Background()).Feature(FeaturePaneRead)
	if second.State != FeatureSupported || second.Reason != "recognized_validation_refusal" {
		t.Fatalf("second check = %+v, want supported/recognized_validation_refusal", second)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestPaneReadCapabilityProbeClassifiesEvidence(t *testing.T) {
	for _, test := range []struct {
		name     string
		response map[string]any
		state    FeatureState
		reason   string
	}{
		{"recognized", map[string]any{"error": map[string]any{"code": "pane_not_found"}}, FeatureSupported, "recognized_validation_refusal"},
		{"unknown method", map[string]any{"error": map[string]any{"code": "unknown_method"}}, FeatureUnsupported, "method_not_supported"},
		{"method not found", map[string]any{"error": map[string]any{"code": "method_not_found"}}, FeatureUnsupported, "method_not_supported"},
		{"unsupported method", map[string]any{"error": map[string]any{"code": "unsupported_method"}}, FeatureUnsupported, "method_not_supported"},
		{"generic refusal", map[string]any{"error": map[string]any{"code": "invalid_request"}}, FeatureUnknown, "probe_failed"},
		{"unexpected success", map[string]any{"type": "pane_read"}, FeatureUnknown, "unexpected_probe_success"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path, listener, done := capabilitySocket(t, []map[string]any{test.response})
			defer listener.Close()
			state, reason := probePaneRead(context.Background(), newSocketAPIClient(path))
			if state != test.state || reason != test.reason {
				t.Fatalf("probe = %s/%s, want %s/%s", state, reason, test.state, test.reason)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPaneReadCapabilityProbeDoesNotTargetLivePane(t *testing.T) {
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "herdr.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		var request struct {
			ID     string         `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := json.NewDecoder(conn).Decode(&request); err != nil {
			done <- err
			return
		}
		want := map[string]any{
			"pane_id": "", "source": "visible", "lines": float64(1), "format": "ansi", "strip_ansi": false,
		}
		if request.Method != "pane.read" || !reflect.DeepEqual(request.Params, want) {
			done <- fmt.Errorf("unsafe probe: %+v", request)
			return
		}
		done <- json.NewEncoder(conn).Encode(map[string]any{
			"id": request.ID, "error": map[string]any{"code": "pane_not_found", "message": "pane  not found"},
		})
	}()
	state, reason := probePaneRead(context.Background(), newSocketAPIClient(listener.Addr().String()))
	if state != FeatureSupported || reason != "recognized_validation_refusal" {
		t.Fatalf("probe = %s/%s", state, reason)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestPaneReadCapabilityProbeReportsUnavailableServer(t *testing.T) {
	state, reason := probePaneRead(context.Background(), newSocketAPIClient(filepath.Join(t.TempDir(), "missing.sock")))
	if state != FeatureUnknown || reason != "server_unavailable" {
		t.Fatalf("probe = %s/%s, want unknown/server_unavailable", state, reason)
	}
}

func TestReadPaneCLIFallbackDoesNotHideUnsupportedSocket(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "herdr")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf 'CLI frame'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	refusal := map[string]any{"error": map[string]any{"code": "unknown_method", "message": "unsupported"}}
	path, listener, done := capabilitySocket(t, []map[string]any{refusal, refusal})
	defer listener.Close()
	client := NewClient(bin, path)
	defer client.Close()
	client.noteFeatureSupported(FeaturePaneRead, "operation_succeeded")
	read, err := client.ReadPane(context.Background(), "w1:p1", 1, "ansi")
	if err != nil || string(read.Content) != "CLI frame" {
		t.Fatalf("read = %q, %v", read.Content, err)
	}
	if feature := client.CapabilityStatus().Feature(FeaturePaneRead); feature.State != FeatureUnsupported {
		t.Fatalf("CLI fallback hid socket refusal: %+v", feature)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestVisiblePaneProbeRecordsCapabilityEvidence(t *testing.T) {
	path, done := startUnaryTestSocket(t, "pane.read", map[string]any{
		"type": "pane_read",
		"read": map[string]any{"text": "frame"},
	})
	client := NewClient("/binary/must-not-run", path)
	defer client.Close()
	client.InvalidateLiveCapabilities()
	content, err := client.ProbePaneVisible(context.Background(), "w1:p1", 1, "ansi")
	if err != nil || string(content) != "frame" {
		t.Fatalf("probe = %q, %v", content, err)
	}
	if !client.SupportsPaneRead() {
		t.Fatalf("successful visible probe left pane.read unknown: %+v", client.CapabilityStatus().Feature(FeaturePaneRead))
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
