package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCapabilityRefreshTracksLiveServerSeparatelyFromInstalledClient(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "herdr")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' 'herdr 0.7.5'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	socketPath, listener, done := capabilitySocket(t, []map[string]any{
		{"type": "pong", "version": "0.9.0", "protocol": 1},
		{"error": map[string]any{"code": "workspace_move_block_failed", "message": "empty selection"}},
		{"error": map[string]any{"code": "tab_not_found", "message": "tab  not found"}},
		{"error": map[string]any{"code": "pane_not_found", "message": "empty pane"}},
		{"type": "pong", "version": "0.9.0", "protocol": 1},
		{"error": map[string]any{"code": "unknown_method", "message": "unsupported"}},
	})
	defer listener.Close()

	client := NewClient(bin, socketPath)
	first := client.RefreshCapabilities(context.Background())
	if first.InstalledClientVersion != "herdr 0.7.5" || first.ServerVersion != "0.9.0" {
		t.Fatalf("status = %+v, want separate installed and running versions", first)
	}
	if !first.Supports(FeatureWorkspaceMoveBlock) {
		t.Fatalf("first status = %+v, want live move-block support", first)
	}

	second := client.RefreshCapabilities(context.Background())
	if second.ServerVersion != "0.9.0" || second.Supports(FeatureWorkspaceMoveBlock) {
		t.Fatalf("second status = %+v, want same-version server with unsupported move-block", second)
	}
	if second.Feature(FeatureWorkspaceMoveBlock).Reason != "method_not_supported" {
		t.Fatalf("move-block evidence = %+v, want method_not_supported", second.Feature(FeatureWorkspaceMoveBlock))
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCapabilityRefreshLeavesGenericProbeFailureUnknown(t *testing.T) {
	socketPath, listener, done := capabilitySocket(t, []map[string]any{
		{"type": "pong", "version": "0.9.0", "protocol": 1},
		{"error": map[string]any{"code": "invalid_request", "message": "not a capability probe"}},
		{"error": map[string]any{"code": "tab_not_found", "message": "tab  not found"}},
		{"error": map[string]any{"code": "pane_not_found", "message": "empty pane"}},
	})
	defer listener.Close()

	status := NewClient("missing-herdr", socketPath).RefreshCapabilities(context.Background())
	feature := status.Feature(FeatureWorkspaceMoveBlock)
	if feature.State != FeatureUnknown || feature.Reason == "method_not_supported" {
		t.Fatalf("move-block evidence = %+v, want unknown generic probe failure", feature)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCapabilityHealthyRefreshRetainsLearnedEvidence(t *testing.T) {
	socketPath, listener, done := capabilitySocket(t, []map[string]any{
		{"type": "pong", "version": "0.9.0", "protocol": 1},
		{"error": map[string]any{"code": "workspace_move_block_failed", "message": "empty selection"}},
	})
	defer listener.Close()

	client := NewClient("missing-herdr", socketPath)
	client.NoteWorkspaceReorderedSupported()
	client.noteFeatureSupported(FeaturePaneRead, "operation_succeeded")
	client.noteFeatureSupported(FeatureTabMove, "operation_succeeded")
	status := client.RefreshCapabilities(context.Background())
	for _, feature := range []string{FeaturePaneRead, FeatureTabMove, FeatureWorkspaceReordered} {
		if !status.Supports(feature) {
			t.Errorf("healthy refresh erased %s: %+v", feature, status.Feature(feature))
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCapabilityProbeRejectsUnrelatedSuccessfulResult(t *testing.T) {
	socketPath, listener, done := capabilitySocket(t, []map[string]any{
		{"type": "pong", "version": "0.9.0", "protocol": 1},
		{"type": "unrelated_result"},
		{"error": map[string]any{"code": "tab_not_found", "message": "empty tab"}},
		{"error": map[string]any{"code": "pane_not_found", "message": "empty pane"}},
	})
	defer listener.Close()

	status := NewClient("missing-herdr", socketPath).RefreshCapabilities(context.Background())
	if status.Supports(FeatureWorkspaceMoveBlock) {
		t.Fatalf("unrelated result falsely advertised move_block: %+v", status.Feature(FeatureWorkspaceMoveBlock))
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCapabilityRefreshDiscoversTabMoveFromValidationRefusal(t *testing.T) {
	socketPath, listener, done := capabilitySocket(t, []map[string]any{
		{"type": "pong", "version": "0.9.0", "protocol": 1},
		{"error": map[string]any{"code": "workspace_move_block_failed", "message": "empty selection"}},
		{"error": map[string]any{"code": "tab_not_found", "message": "empty tab"}},
		{"error": map[string]any{"code": "pane_not_found", "message": "empty pane"}},
	})
	defer listener.Close()

	status := NewClient("missing-herdr", socketPath).RefreshCapabilities(context.Background())
	if !status.Supports(FeatureTabMove) {
		t.Fatalf("tab.move validation refusal did not publish support: %+v", status.Feature(FeatureTabMove))
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCapabilitySameVersionRestartClearsLearnedEvidence(t *testing.T) {
	socketPath, listener, done := capabilitySocket(t, []map[string]any{
		{"type": "pong", "version": "0.9.0", "protocol": 1},
		{"error": map[string]any{"code": "workspace_move_block_failed", "message": "empty selection"}},
		{"error": map[string]any{"code": "tab_not_found", "message": "empty tab"}},
		{"error": map[string]any{"code": "pane_not_found", "message": "empty pane"}},
		{"type": "pong", "version": "0.9.0", "protocol": 1},
		{"error": map[string]any{"code": "workspace_move_block_failed", "message": "empty selection"}},
		{"error": map[string]any{"code": "tab_not_found", "message": "empty tab"}},
		{"error": map[string]any{"code": "unknown_method", "message": "unsupported"}},
	})
	defer listener.Close()

	client := NewClient("missing-herdr", socketPath)
	client.RefreshCapabilities(context.Background())
	client.noteFeatureSupported(FeaturePaneRead, "operation_succeeded")
	client.NoteWorkspaceReorderedSupported()
	client.InvalidateLiveCapabilities()
	status := client.RefreshCapabilities(context.Background())
	for _, feature := range []string{FeaturePaneRead, FeatureWorkspaceReordered} {
		if status.Supports(feature) {
			t.Errorf("same-version restart retained %s: %+v", feature, status.Feature(feature))
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
func TestCapabilityRefreshDropsStaleInFlightResultAfterInvalidation(t *testing.T) {
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(t.TempDir(), "herdr.sock"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	receivedPing := make(chan struct{})
	releasePing := make(chan struct{})
	serverErr := make(chan error, 1)
	go func() {
		defer listener.Close()
		for _, method := range []string{"ping", "workspace.move_block", "tab.move", "pane.read"} {
			conn, acceptErr := listener.AcceptUnix()
			if acceptErr != nil {
				serverErr <- acceptErr
				return
			}
			var request struct {
				ID     string `json:"id"`
				Method string `json:"method"`
			}
			decodeErr := json.NewDecoder(bufio.NewReader(conn)).Decode(&request)
			if decodeErr != nil {
				_ = conn.Close()
				serverErr <- decodeErr
				return
			}
			if request.Method != method {
				_ = conn.Close()
				serverErr <- fmt.Errorf("method = %q, want %q", request.Method, method)
				return
			}
			if method == "ping" {
				close(receivedPing)
				<-releasePing
			}
			response := map[string]any{"id": request.ID}
			if method == "ping" {
				response["result"] = map[string]any{"type": "pong", "version": "0.9.0", "protocol": 1}
			} else if method == "workspace.move_block" {
				response["error"] = map[string]any{"code": "workspace_move_block_failed", "message": "empty selection"}
			} else if method == "tab.move" {
				response["error"] = map[string]any{"code": "tab_not_found", "message": "empty tab"}
			} else {
				response["error"] = map[string]any{"code": "pane_not_found", "message": "empty pane"}
			}
			if encodeErr := json.NewEncoder(conn).Encode(response); encodeErr != nil {
				_ = conn.Close()
				serverErr <- encodeErr
				return
			}
			_ = conn.Close()
		}
		serverErr <- nil
	}()

	client := NewClient("missing-herdr", listener.Addr().String())
	client.NoteWorkspaceReorderedSupported()
	refreshDone := make(chan struct{})
	go func() {
		client.RefreshCapabilities(context.Background())
		close(refreshDone)
	}()
	select {
	case <-receivedPing:
	case <-time.After(time.Second):
		t.Fatal("refresh did not reach ping")
	}
	client.InvalidateLiveCapabilities()
	close(releasePing)
	select {
	case <-refreshDone:
	case <-time.After(time.Second):
		t.Fatal("refresh did not finish")
	}
	status := client.CapabilityStatus()
	if feature := status.Feature(FeatureWorkspaceReordered); feature.State != FeatureUnknown ||
		feature.Reason != "reconnect_required" {
		t.Fatalf("stale refresh restored workspace reorder: %+v", feature)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}
func TestCapabilityInitialEpochObservationIsRejectedAfterInvalidation(t *testing.T) {
	client := NewClient("/missing/review-herdr", "")
	oldEpoch := client.capabilityEpoch()
	client.InvalidateLiveCapabilities()
	client.noteFeatureSupportedAt(oldEpoch, FeaturePaneRead, "operation_succeeded")
	if client.SupportsPaneRead() {
		t.Fatalf("old initial epoch %d restored pane.read after invalidation to epoch %d", oldEpoch, client.capabilityEpoch())
	}
}

func TestCapabilityPingIdentityChangeRejectsOlderOperationEvidence(t *testing.T) {
	path, listener, done := capabilitySocket(t, []map[string]any{
		{"type": "pong", "version": "0.8.0", "protocol": 1},
		{"error": map[string]any{"code": "workspace_move_block_failed", "message": "empty selection"}},
		{"error": map[string]any{"code": "tab_not_found", "message": "empty tab"}},
		{"error": map[string]any{"code": "pane_not_found", "message": "empty pane"}},
		{"type": "pong", "version": "0.9.0", "protocol": 2},
		{"error": map[string]any{"code": "workspace_move_block_failed", "message": "empty selection"}},
		{"error": map[string]any{"code": "tab_not_found", "message": "empty tab"}},
		{"error": map[string]any{"code": "unknown_method", "message": "unsupported"}},
	})
	defer listener.Close()
	client := NewClient("/missing/review-herdr", path)
	client.InvalidateLiveCapabilities()
	before := client.RefreshCapabilities(context.Background())
	oldEpoch := client.capabilityEpoch()
	after := client.RefreshCapabilities(context.Background())
	if before.ServerVersion != "0.8.0" || after.ServerVersion != "0.9.0" {
		t.Fatalf("fixture identity was not replaced: before=%+v after=%+v", before, after)
	}
	client.noteFeatureSupportedAt(oldEpoch, FeaturePaneRead, "operation_succeeded")
	if client.SupportsPaneRead() {
		t.Errorf("old server's operation evidence was accepted after ping identified a different server: epoch before=%d after=%d", oldEpoch, client.capabilityEpoch())
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func capabilitySocket(t *testing.T, responses []map[string]any) (string, *net.UnixListener, <-chan error) {
	t.Helper()
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(t.TempDir(), "herdr.sock"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		defer listener.Close()
		for _, response := range responses {
			conn, acceptErr := listener.AcceptUnix()
			if acceptErr != nil {
				done <- acceptErr
				return
			}
			func() {
				defer conn.Close()
				var request struct {
					ID     string `json:"id"`
					Method string `json:"method"`
				}
				if decodeErr := json.NewDecoder(bufio.NewReader(conn)).Decode(&request); decodeErr != nil {
					done <- decodeErr
					return
				}
				envelope := map[string]any{"id": request.ID}
				if failure, ok := response["error"]; ok {
					envelope["error"] = failure
				} else {
					envelope["result"] = response
				}
				if encodeErr := json.NewEncoder(conn).Encode(envelope); encodeErr != nil {
					return
				}
			}()
		}
		done <- nil
	}()
	return listener.Addr().String(), listener, done
}
